// Package installer publishes a verified release through a permanent kernel gate.
// It does not fetch, execute host configuration commands, or discover sessions.
package installer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
)

const (
	gateName     = ".install.lock"
	imageName    = "image"
	manifestName = "plugins/codex/tmag/.codex-plugin/plugin.json"
	maxImage     = 128 << 20
)

var (
	ErrBusy          = errors.New("another installer owns the publication gate")
	ErrOwnership     = errors.New("installer ownership is uncertain; stop legacy installers before inspecting and recovering the lock")
	ErrVersion       = errors.New("release version does not match the current plugin checkout")
	versionPattern   = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	workspacePattern = regexp.MustCompile(`^\.install-work-[a-f0-9]{32}$`)
)

type identity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

func identify(st os.FileInfo) identity {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return identity{}
	}
	return identity{uint64(s.Dev), s.Ino} //nolint:unconvert // Darwin Dev is signed; Linux Dev is uint64.
}

func owned(st os.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && s.Uid == uint32(os.Geteuid())
}

type binding struct {
	Protocol  string   `json:"protocol"`
	Plugin    identity `json:"plugin"`
	Bin       identity `json:"bin"`
	Workspace string   `json:"workspace"`
	Work      identity `json:"work"`
}

// Config contains the release claim already verified before this binary starts.
// Live is checked before admission and immediately before the accepted rename.
type Config struct {
	Root     string
	Want     string
	Compiled string
	Source   string
	Digest   string
	Live     func(context.Context) error
}

type namespace struct {
	path                                    string
	plugin, bin, work                       *os.Root
	pluginInfo, binInfo, workInfo, gateInfo os.FileInfo
	gate                                    *os.File
	binding                                 binding
	beforeLink                              func() // Optional creation barrier for deterministic namespace tests.
}

func (n *namespace) close() {
	if n.work != nil {
		_ = n.work.Close()
	}
	if n.gate != nil {
		_ = n.gate.Close()
	}
	if n.bin != nil {
		_ = n.bin.Close()
	}
	if n.plugin != nil {
		_ = n.plugin.Close()
	}
}

func regular(root *os.Root, name string, flags int) (*os.File, error) {
	st, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || !owned(st) {
		return nil, ErrOwnership
	}
	f, err := root.OpenFile(name, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	actual, err := f.Stat()
	if err != nil || !os.SameFile(st, actual) {
		_ = f.Close()
		return nil, ErrOwnership
	}
	return f, nil
}

func (n *namespace) validateBase(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	st, err := os.Stat(n.path)
	if err != nil || !st.IsDir() || !owned(st) || !os.SameFile(st, n.pluginInfo) {
		return ErrOwnership
	}
	st, err = n.plugin.Lstat("bin")
	if err != nil || !st.IsDir() || !owned(st) || !os.SameFile(st, n.binInfo) {
		return ErrOwnership
	}
	return nil
}

func (n *namespace) validate(ctx context.Context) error {
	if err := n.validateBase(ctx); err != nil {
		return err
	}
	for _, check := range []struct {
		root *os.Root
		name string
		info os.FileInfo
	}{
		{n.bin, gateName, n.gateInfo},
		{n.bin, n.binding.Workspace, n.workInfo},
		{n.work, "gate", n.gateInfo},
	} {
		st, err := check.root.Lstat(check.name)
		if err != nil || !os.SameFile(st, check.info) {
			return ErrOwnership
		}
	}
	return nil
}

func openNamespace(ctx context.Context, path string) (*namespace, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return nil, err
	}
	n := &namespace{path: canonical}
	ok := false
	defer func() {
		if !ok {
			n.close()
		}
	}()
	n.plugin, err = os.OpenRoot(canonical)
	if err != nil {
		return nil, err
	}
	n.pluginInfo, err = n.plugin.Stat(".")
	if err != nil || !owned(n.pluginInfo) {
		return nil, ErrOwnership
	}
	if err = n.plugin.Mkdir("bin", 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	n.binInfo, err = n.plugin.Lstat("bin")
	if err != nil || !n.binInfo.IsDir() || !owned(n.binInfo) {
		return nil, ErrOwnership
	}
	n.bin, err = n.plugin.OpenRoot("bin")
	if err != nil {
		return nil, err
	}
	actual, err := n.bin.Stat(".")
	if err != nil || !os.SameFile(actual, n.binInfo) {
		return nil, ErrOwnership
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if _, err = n.bin.Lstat(gateName); errors.Is(err, os.ErrNotExist) {
		err = n.createGate(ctx)
	}
	if err != nil {
		return nil, err
	}
	n.gate, err = regular(n.bin, gateName, os.O_RDWR)
	if err != nil {
		return nil, ErrOwnership
	}
	if err = syscall.Flock(int(n.gate.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	n.gateInfo, err = n.gate.Stat()
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(n.gate, 4097))
	if err != nil || len(data) > 4096 || json.Unmarshal(data, &n.binding) != nil {
		return nil, ErrOwnership
	}
	b := n.binding
	if b.Protocol != "tmag/install/v1" || b.Plugin != identify(n.pluginInfo) || b.Bin != identify(n.binInfo) ||
		!workspacePattern.MatchString(b.Workspace) {
		return nil, ErrOwnership
	}
	n.workInfo, err = n.bin.Lstat(b.Workspace)
	if err != nil || !n.workInfo.IsDir() || !owned(n.workInfo) || identify(n.workInfo) != b.Work {
		return nil, ErrOwnership
	}
	n.work, err = n.bin.OpenRoot(b.Workspace)
	if err != nil {
		return nil, err
	}
	actual, err = n.work.Stat(".")
	if err != nil || !os.SameFile(actual, n.workInfo) {
		return nil, ErrOwnership
	}
	actual, err = n.work.Lstat("gate")
	if err != nil || !os.SameFile(actual, n.gateInfo) {
		return nil, ErrOwnership
	}
	if err = n.validate(ctx); err != nil {
		return nil, err
	}
	ok = true
	return n, nil
}

// createGate atomically publishes a complete binding; Link cannot replace even
// an empty legacy directory. A pre-link crash leaves only unregistered scratch.
func (n *namespace) createGate(ctx context.Context) error {
	if err := n.validateBase(ctx); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	name := ".install-work-" + hex.EncodeToString(nonce[:])
	if err := n.bin.Mkdir(name, 0o700); err != nil {
		return err
	}
	work, err := n.bin.OpenRoot(name)
	if err != nil {
		return err
	}
	defer work.Close()
	st, err := work.Stat(".")
	if err != nil {
		return err
	}
	b := binding{"tmag/install/v1", identify(n.pluginInfo), identify(n.binInfo), name, identify(st)}
	f, err := work.OpenFile("gate", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err
	}
	// The gate inode stays pinned by f until its stable link exists.
	if n.beforeLink != nil {
		n.beforeLink()
	}
	// Root.Link uses a relative pathname. Revalidate the initialized source
	// after any intervening work, not merely after a possibly foreign link was
	// already committed. No portable atomic conditional-link primitive exists
	// against a malicious continuously racing same-user namespace mutator.
	if err = n.validateBase(ctx); err != nil {
		return err
	}
	current, err := n.bin.Lstat(name)
	if err != nil || !current.IsDir() || !owned(current) || !os.SameFile(current, st) {
		return ErrOwnership
	}
	held, err := f.Stat()
	if err != nil || !held.Mode().IsRegular() || !owned(held) || held.Size() != int64(len(data)) {
		return ErrOwnership
	}
	for _, position := range []struct {
		root *os.Root
		name string
	}{{work, "gate"}, {n.bin, name + "/gate"}} {
		current, err = position.root.Lstat(position.name)
		if err != nil || !os.SameFile(current, held) {
			return ErrOwnership
		}
	}
	initialized := make([]byte, len(data))
	if _, err = f.ReadAt(initialized, 0); err != nil || !bytes.Equal(initialized, data) {
		return ErrOwnership
	}
	err = n.bin.Link(name+"/gate", gateName)
	if errors.Is(err, os.ErrExist) {
		// Only these exclusively created objects belong to the losing creator.
		current, statErr := n.bin.Lstat(name)
		gate, gateErr := work.Lstat("gate")
		created, createdErr := f.Stat()
		if statErr == nil && gateErr == nil && createdErr == nil && os.SameFile(current, st) && os.SameFile(gate, created) {
			_ = work.Remove("gate")
			_ = n.bin.Remove(name)
		}
		return nil
	}
	if err != nil {
		return err
	}
	installed, err := n.bin.Lstat(gateName)
	if err != nil || !os.SameFile(installed, held) {
		return ErrOwnership
	}
	return nil
}

func checkoutVersion(root *os.Root, want string) error {
	f, err := regular(root, manifestName, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(data) > 65536 {
		return ErrVersion
	}
	var m struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(data, &m) != nil || m.Version != want {
		return ErrVersion
	}
	return nil
}

func (n *namespace) recoverImage() error {
	entries, err := n.work.Open(".")
	if err != nil {
		return err
	}
	defer entries.Close()
	items, err := entries.ReadDir(3)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	for _, e := range items {
		if e.Name() != "gate" && e.Name() != imageName {
			return ErrOwnership
		}
	}
	// The immutable gate reserves imageName in this private bound workspace.
	st, err := n.work.Lstat(imageName)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || !owned(st) {
		return ErrOwnership
	}
	return n.work.Remove(imageName)
}

// Publish has one commit point: same-filesystem rename while the permanent gate
// is held. Cancellation observed before that point refuses the action; a signal
// racing the final syscall can yield an indeterminate caller result.
func Publish(ctx context.Context, c Config) error {
	if !versionPattern.MatchString(c.Want) || c.Want != c.Compiled {
		return ErrVersion
	}
	if len(c.Digest) != 64 {
		return ErrOwnership
	}
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.Live != nil {
			return c.Live(ctx)
		}
		return nil
	}
	if err := check(); err != nil {
		return err
	}
	n, err := openNamespace(ctx, c.Root)
	if err != nil {
		return err
	}
	defer n.close()
	if err = checkoutVersion(n.plugin, c.Want); err != nil {
		return err
	}
	if err = n.recoverImage(); err != nil {
		return err
	}
	source, err := os.OpenFile(c.Source, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer source.Close()
	sourceInfo, err := source.Stat()
	if err != nil || !sourceInfo.Mode().IsRegular() || sourceInfo.Size() <= 0 || sourceInfo.Size() > maxImage {
		return ErrOwnership
	}
	image, err := n.work.OpenFile(imageName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = image.Close()
		}
	}()
	buffer := make([]byte, 128<<10)
	digest := sha256.New()
	var size int64
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		count, readErr := source.Read(buffer)
		size += int64(count)
		if size > maxImage {
			return fmt.Errorf("release executable exceeds size limit")
		}
		if count > 0 {
			if _, err = image.Write(buffer[:count]); err != nil {
				return err
			}
			_, _ = digest.Write(buffer[:count])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if size != sourceInfo.Size() {
		return ErrOwnership
	}
	if hex.EncodeToString(digest.Sum(nil)) != c.Digest {
		return ErrOwnership
	}
	if err = image.Chmod(0o755); err != nil {
		return err
	}
	imageInfo, err := image.Stat()
	if err != nil {
		return err
	}
	// Retain an inode pin while closing the writer and through rename. A
	// pathname replacement must not recycle the identity we validate below.
	pin, err := regular(n.work, imageName, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer pin.Close()
	pinned, err := pin.Stat()
	if err != nil || !os.SameFile(pinned, imageInfo) {
		return ErrOwnership
	}
	if err = image.Close(); err != nil {
		return err
	}
	closed = true
	if err = n.validate(ctx); err != nil {
		return err
	}
	if err = check(); err != nil {
		return err
	}
	if err = n.validate(ctx); err != nil {
		return err
	}
	if err = checkoutVersion(n.plugin, c.Want); err != nil {
		return err
	}
	st, err := n.work.Lstat(imageName)
	if err != nil || !st.Mode().IsRegular() || !owned(st) || !os.SameFile(st, imageInfo) {
		return ErrOwnership
	}
	if st, err = n.bin.Lstat("tmag"); err == nil {
		if !st.Mode().IsRegular() || !owned(st) {
			return ErrOwnership
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return n.bin.Rename(n.binding.Workspace+"/"+imageName, "tmag")
}
