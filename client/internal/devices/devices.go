// Package devices keeps the devices paired with the gateway: each device's
// credential as a digest, its push subscription, and the invitations that pair
// new devices. The file holds no secret: a token or an invitation code exists
// only in the process that issued it and with whoever received it.
//
// Changes take a lock on the directory and replace the file atomically, so the
// gateway and `tmag-client auth` change it safely side by side. Reads use a
// copy that is refreshed whenever the file changes, so a running gateway sees
// a revocation at its next request.
package devices

import (
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

// Errors a caller tells apart.
var (
	ErrUnauthorized = errors.New("credential is invalid, expired or revoked")
	ErrInvitation   = errors.New("invitation code is invalid, expired or already used")
	ErrNotFound     = errors.New("no such device")
)

const (
	// InvitationLife is how long an invitation code may be redeemed.
	InvitationLife = 10 * time.Minute
	// maxInvitations bounds the codes waiting to be redeemed.
	maxInvitations = 16
	// maxDevices bounds the paired devices.
	maxDevices = 64
	// maxLabel bounds a device's label, in bytes.
	maxLabel = 64
	// fileName is the store's file in its directory, lockName its lock.
	fileName = "devices.json"
	lockName = "devices.lock"
	// tokenPrefix marks a gateway credential, for people and secret scanners.
	tokenPrefix = "tmagc_"
	// crockford is Crockford's base32 alphabet: no I, L, O or U to misread.
	crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

// Device is one paired device, as the gateway knows it.
type Device struct {
	ID      string        `json:"id"`
	Label   string        `json:"label"`
	Created time.Time     `json:"created"`
	Expires *time.Time    `json:"expires,omitempty"`
	Push    *Subscription `json:"push,omitempty"`
}

// Subscription is a device's Web Push subscription: where its browser's push
// service takes messages, and the keys that encrypt them for it.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	P256DH   string `json:"p256dh"`
	Auth     string `json:"auth"`
}

// Invitation pairs one device: its code works once, until Expires.
type Invitation struct {
	Code    string    `json:"code"`
	Label   string    `json:"label"`
	Expires time.Time `json:"expires"`
}

type device struct {
	Device
	Digest string `json:"digest"`
}

type invitation struct {
	Digest  string        `json:"digest"`
	Label   string        `json:"label"`
	Expires time.Time     `json:"expires"`
	Life    time.Duration `json:"life,omitempty"` // the credential's, zero until revoked
}

type file struct {
	Devices     []device     `json:"devices"`
	Invitations []invitation `json:"invitations,omitempty"`
}

// Store is the devices of one gateway, in its state directory.
type Store struct {
	dir string
	now func() time.Time

	mu     sync.Mutex
	cached file
	stamp  stamp
}

// stamp tells one version of the file from another.
type stamp struct {
	ok    bool
	mod   time.Time
	size  int64
	inode uint64
}

// Open returns the store in dir, creating the directory for its owner alone.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir, now: time.Now}, nil
}

// Invite creates a single-use invitation for a device called label, or named
// by the device that redeems it when label is empty, whose credential lasts
// life, or until revoked when life is zero.
func (s *Store) Invite(label string, life time.Duration) (Invitation, error) {
	label, err := cleanLabel(label, true)
	if err != nil {
		return Invitation{}, err
	}
	if life < 0 {
		return Invitation{}, errors.New("a credential's life cannot be negative")
	}
	code, err := newCode()
	if err != nil {
		return Invitation{}, err
	}
	inv := Invitation{Code: code, Label: label, Expires: s.now().Add(InvitationLife).UTC().Truncate(time.Second)}
	err = s.change(func(f *file) error {
		if len(f.Invitations) >= maxInvitations {
			return fmt.Errorf("%d invitations are waiting; redeem one or let them expire", maxInvitations)
		}
		f.Invitations = append(f.Invitations, invitation{Digest: digest(normalCode(code)), Label: label, Expires: inv.Expires, Life: life})
		return nil
	})
	return inv, err
}

// Redeem spends an invitation and returns the device it pairs with the
// token that proves it. Two devices can never redeem one code. named is what
// the device calls itself, its label when the invitation has none.
func (s *Store) Redeem(code, named string) (Device, string, error) {
	want := digest(normalCode(code))
	if l, err := cleanLabel(named, false); err == nil {
		named = l
	} else {
		named = "device"
	}
	token := tokenPrefix + randomString(32)
	var paired Device
	err := s.change(func(f *file) error {
		i := slices.IndexFunc(f.Invitations, func(inv invitation) bool { return inv.Digest == want })
		if i < 0 {
			return ErrInvitation
		}
		inv := f.Invitations[i]
		f.Invitations = slices.Delete(f.Invitations, i, i+1)
		if len(f.Devices) >= maxDevices {
			return fmt.Errorf("%d devices are paired; revoke one first", maxDevices)
		}
		now := s.now().UTC().Truncate(time.Second)
		paired = Device{ID: newID(f), Label: cmp.Or(inv.Label, named), Created: now}
		if inv.Life > 0 {
			end := now.Add(inv.Life)
			paired.Expires = &end
		}
		f.Devices = append(f.Devices, device{Device: paired, Digest: digest(token)})
		return nil
	})
	if err != nil {
		return Device{}, "", err
	}
	return paired, token, nil
}

// Authenticate returns the device a token proves.
func (s *Store) Authenticate(token string) (Device, error) {
	if !strings.HasPrefix(token, tokenPrefix) {
		return Device{}, ErrUnauthorized
	}
	f, err := s.read()
	if err != nil {
		return Device{}, err
	}
	want := digest(token)
	now := s.now()
	for _, d := range f.Devices {
		if d.Digest == want && live(d.Device, now) {
			return d.Device, nil
		}
	}
	return Device{}, ErrUnauthorized
}

// Device returns the paired device id, if it is still paired.
func (s *Store) Device(id string) (Device, bool, error) {
	f, err := s.read()
	if err != nil {
		return Device{}, false, err
	}
	now := s.now()
	for _, d := range f.Devices {
		if d.ID == id && live(d.Device, now) {
			return d.Device, true, nil
		}
	}
	return Device{}, false, nil
}

// Devices lists the paired devices, oldest first.
func (s *Store) Devices() ([]Device, error) {
	f, err := s.read()
	if err != nil {
		return nil, err
	}
	now := s.now()
	var out []Device
	for _, d := range f.Devices {
		if live(d.Device, now) {
			out = append(out, d.Device)
		}
	}
	return out, nil
}

// Revoke unpairs a device: its credential and push subscription go.
func (s *Store) Revoke(id string) error {
	return s.change(func(f *file) error {
		i := slices.IndexFunc(f.Devices, func(d device) bool { return d.ID == id })
		if i < 0 {
			return ErrNotFound
		}
		f.Devices = slices.Delete(f.Devices, i, i+1)
		return nil
	})
}

// Subscribe keeps sub as the device's push subscription, replacing any other.
func (s *Store) Subscribe(id string, sub Subscription) error {
	return s.change(func(f *file) error {
		i := slices.IndexFunc(f.Devices, func(d device) bool { return d.ID == id })
		if i < 0 {
			return ErrNotFound
		}
		f.Devices[i].Push = &sub
		return nil
	})
}

// Unsubscribe drops the device's push subscription when its endpoint is
// endpoint, or whatever it is when endpoint is empty: a push service's late
// refusal of an old endpoint never drops a newer subscription.
func (s *Store) Unsubscribe(id, endpoint string) error {
	return s.change(func(f *file) error {
		i := slices.IndexFunc(f.Devices, func(d device) bool { return d.ID == id })
		if i < 0 {
			return ErrNotFound
		}
		if p := f.Devices[i].Push; p != nil && (endpoint == "" || p.Endpoint == endpoint) {
			f.Devices[i].Push = nil
		}
		return nil
	})
}

func live(d Device, now time.Time) bool { return d.Expires == nil || now.Before(*d.Expires) }

// read returns the file as it is now, from the copy while the file is
// unchanged.
func (s *Store) read() (file, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, fileName)
	st, err := stampOf(path)
	if err != nil {
		return file{}, err
	}
	if st == s.stamp {
		return s.cached, nil
	}
	f, err := load(path)
	if err != nil {
		return file{}, err
	}
	s.cached, s.stamp = f, st
	return f, nil
}

// change applies fn to the file under the directory's lock and replaces the
// file with the result, dropping what has expired. When fn fails, nothing
// changes.
func (s *Store) change(fn func(*file) error) error {
	lock, err := os.OpenFile(filepath.Join(s.dir, lockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	path := filepath.Join(s.dir, fileName)
	f, err := load(path)
	if err != nil {
		return err
	}
	now := s.now()
	f.Invitations = slices.DeleteFunc(f.Invitations, func(inv invitation) bool { return !now.Before(inv.Expires) })
	f.Devices = slices.DeleteFunc(f.Devices, func(d device) bool { return !live(d.Device, now) })
	if err := fn(&f); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(path, append(b, '\n')); err != nil {
		return err
	}
	s.mu.Lock()
	s.stamp = stamp{} // read again: the next read sees this change
	s.mu.Unlock()
	return nil
}

func load(path string) (file, error) {
	var f file
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

func stampOf(path string) (stamp, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return stamp{ok: true}, nil
	}
	if err != nil {
		return stamp{}, err
	}
	st := stamp{ok: true, mod: fi.ModTime(), size: fi.Size()}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		st.inode = sys.Ino
	}
	return st, nil
}

// writeFile replaces path with b atomically, readable by its owner alone.
func writeFile(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func cleanLabel(label string, empty bool) (string, error) {
	label = strings.TrimSpace(label)
	switch {
	case label == "" && empty:
		return "", nil
	case label == "":
		return "", errors.New("a device needs a label, such as phone")
	case len(label) > maxLabel:
		return "", fmt.Errorf("a label has at most %d bytes", maxLabel)
	case strings.IndexFunc(label, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0:
		return "", errors.New("a label is printable text")
	}
	return label, nil
}

// newCode returns twelve characters of Crockford's alphabet, in three groups.
func newCode() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	var sb strings.Builder
	for i, c := range b {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(crockford[int(c)%len(crockford)]) // 256 is a multiple of 32
	}
	return sb.String(), nil
}

// normalCode reads a code as people type it: any case, with or without
// separators, and the letters Crockford's alphabet reads as digits.
func normalCode(code string) string {
	var sb strings.Builder
	for _, r := range strings.ToUpper(code) {
		switch r {
		case '-', ' ', '\t':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func newID(f *file) string {
	for {
		id := hex.EncodeToString(randomBytes(4))
		if !slices.ContainsFunc(f.Devices, func(d device) bool { return d.ID == id }) {
			return id
		}
	}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b) // never fails: crypto/rand panics rather than return an error
	return b
}

func randomString(n int) string { return base64.RawURLEncoding.EncodeToString(randomBytes(n)) }

func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
