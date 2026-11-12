// Package auth manages the credentials that pair a device with tmag-client's
// gateway. Its directory holds digests and metadata, never a token.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var ErrUnauthorized = errors.New("credential is invalid, expired or revoked")

// Store is the gateway's authorization directory.
type Store struct{ Dir string }

// Credential is one device's admission. Push reports whether the device has
// a push subscription.
type Credential struct {
	ID      string     `json:"id"`
	Label   string     `json:"label"`
	Created time.Time  `json:"created"`
	Expires *time.Time `json:"expires,omitempty"`
	Push    bool       `json:"push"`
}

// Subscription is a device's Web Push subscription.
type Subscription struct {
	Endpoint string    `json:"endpoint"`
	P256DH   string    `json:"p256dh"`
	Auth     string    `json:"auth"`
	Created  time.Time `json:"created"`
}

// Generated is a credential together with the token that proves it. The
// token exists only in this value and in the device that received it.
type Generated struct {
	Credential
	Token string `json:"token"`
}

// Invitation pairs one device. Its code lasts ten minutes and works once.
type Invitation struct {
	Code    string    `json:"code"`
	Expires time.Time `json:"expires"`
}

type record struct {
	ID           string        `json:"id"`
	Label        string        `json:"label"`
	Created      time.Time     `json:"created"`
	Expires      *time.Time    `json:"expires,omitempty"`
	Digest       string        `json:"digest"`
	Subscription *Subscription `json:"subscription,omitempty"`
}

type invitation struct {
	Digest  string        `json:"digest"`
	Label   string        `json:"label"`
	Expires time.Time     `json:"expires"`
	TTL     time.Duration `json:"ttl"`
}

type database struct {
	Version     int          `json:"version"`
	Instance    string       `json:"instance"`
	Credentials []record     `json:"credentials"`
	Invitations []invitation `json:"invitations,omitempty"`
}

// Entry is one subscribed device, as the push sender needs it.
type Entry struct {
	ID           string
	Subscription Subscription
}

const (
	// maxInvitations bounds codes that wait to be redeemed.
	maxInvitations = 16
	// credentialLife is how long an invitation's code may be redeemed.
	credentialLife = 10 * time.Minute
)

// crockford is Crockford's base32 alphabet: no I, L, O or U to misread.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func code() (string, error) {
	var b [12]byte
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(crockford))))
		if err != nil {
			return "", err
		}
		b[i] = crockford[n.Int64()]
	}
	return string(b[:]), nil
}

// grouped writes a code as people read it: XXXX-XXXX-XXXX.
func grouped(code string) string { return code[:4] + "-" + code[4:8] + "-" + code[8:] }

// SameCode reports whether two spellings name the same invitation code.
func SameCode(a, b string) bool { return normalized(a) == normalized(b) }

// normalized reads a code as people type it: in either case, with dashes or
// spaces, O for 0 and I or L for 1.
func normalized(code string) string {
	var b strings.Builder
	for _, c := range strings.ToUpper(code) {
		switch c {
		case '-', ' ':
		case 'O':
			b.WriteRune('0')
		case 'I', 'L':
			b.WriteRune('1')
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

func valid(r record, now time.Time) bool { return r.Expires == nil || now.Before(*r.Expires) }

func labelOK(label string, ttl time.Duration) error {
	if strings.TrimSpace(label) == "" || len(label) > 128 || strings.ContainsAny(label, "\r\n\x00") {
		return errors.New("a credential needs a label of 1 to 128 characters")
	}
	if ttl < 0 {
		return errors.New("credential lifetime must be positive")
	}
	return nil
}

func (r record) credential() Credential {
	return Credential{ID: r.ID, Label: r.Label, Created: r.Created, Expires: r.Expires, Push: r.Subscription != nil}
}

func issue(d *database, label string, ttl time.Duration) Generated {
	now := time.Now().UTC()
	r := record{ID: random(8), Label: label, Created: now}
	if ttl > 0 {
		t := now.Add(ttl)
		r.Expires = &t
	}
	token := r.ID + "." + random(32)
	r.Digest = digest(token)
	d.Credentials = append(d.Credentials, r)
	return Generated{Credential: r.credential(), Token: token}
}

// with runs fn while holding the store's lock. A write replaces the database
// atomically, so a crash leaves either the old file or the new one.
func (s Store) with(ctx context.Context, write bool, fn func(*database) error) error {
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(s.Dir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(s.Dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := flock(ctx, lock); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	d := database{Version: 1, Credentials: []record{}}
	b, err := os.ReadFile(filepath.Join(s.Dir, "credentials.json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		if write {
			d.Instance = random(16)
		}
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(b, &d); err != nil {
			return fmt.Errorf("read gateway authorization: %w", err)
		}
		if d.Version != 1 || d.Instance == "" {
			return errors.New("unsupported gateway authorization store")
		}
	}
	now := time.Now().UTC()
	live := d.Invitations[:0]
	for _, p := range d.Invitations {
		if now.Before(p.Expires) {
			live = append(live, p)
		}
	}
	d.Invitations = live
	if err := fn(&d); err != nil {
		return err
	}
	if !write {
		return nil
	}
	b, err = json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Dir, ".credentials-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(b, '\n')); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(s.Dir, "credentials.json"))
}

// flock waits for the store's lock, bounded by ctx.
func flock(ctx context.Context, f *os.File) error {
	fd := int(f.Fd())
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			select {
			case <-time.After(10 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		default:
			return err
		}
	}
}

// Pair creates a single-use invitation and returns its code.
func (s Store) Pair(ctx context.Context, label string, ttl time.Duration) (Invitation, error) {
	if err := labelOK(label, ttl); err != nil {
		return Invitation{}, err
	}
	raw, err := code()
	if err != nil {
		return Invitation{}, err
	}
	now := time.Now().UTC()
	out := Invitation{Code: grouped(raw), Expires: now.Add(credentialLife)}
	err = s.with(ctx, true, func(d *database) error {
		d.Invitations = append(d.Invitations, invitation{Digest: digest(raw), Label: label, Expires: out.Expires, TTL: ttl})
		if len(d.Invitations) > maxInvitations {
			d.Invitations = d.Invitations[len(d.Invitations)-maxInvitations:]
		}
		return nil
	})
	return out, err
}

// Redeem exchanges a code for a credential. The code works once.
func (s Store) Redeem(ctx context.Context, raw string) (Generated, error) {
	code := normalized(raw)
	var out Generated
	err := s.with(ctx, true, func(d *database) error {
		now := time.Now().UTC()
		for i, p := range d.Invitations {
			if !equal(p.Digest, digest(code)) {
				continue
			}
			if !now.Before(p.Expires) {
				d.Invitations = append(d.Invitations[:i], d.Invitations[i+1:]...)
				return ErrUnauthorized
			}
			out = issue(d, p.Label, p.TTL)
			d.Invitations = append(d.Invitations[:i], d.Invitations[i+1:]...)
			return nil
		}
		return ErrUnauthorized
	})
	return out, err
}

// Authenticate returns the credential a token proves, if it is still valid.
func (s Store) Authenticate(ctx context.Context, token string) (Credential, error) {
	var out Credential
	id, secret, ok := strings.Cut(token, ".")
	if !ok || len(id) != 16 || len(secret) != 64 {
		return out, ErrUnauthorized
	}
	err := s.with(ctx, false, func(d *database) error {
		for _, r := range d.Credentials {
			if r.ID == id && valid(r, time.Now()) && equal(r.Digest, digest(token)) {
				out = r.credential()
				return nil
			}
		}
		return ErrUnauthorized
	})
	return out, err
}

// List returns every credential, as the person sees them.
func (s Store) List(ctx context.Context) (string, []Credential, error) {
	var instance string
	out := []Credential{}
	err := s.with(ctx, false, func(d *database) error {
		instance = d.Instance
		for _, r := range d.Credentials {
			out = append(out, r.credential())
		}
		return nil
	})
	return instance, out, err
}

// Revoke removes a credential, its streams and its push subscription.
func (s Store) Revoke(ctx context.Context, id string) error {
	return s.with(ctx, true, func(d *database) error {
		for i, r := range d.Credentials {
			if r.ID == id {
				d.Credentials = append(d.Credentials[:i], d.Credentials[i+1:]...)
				return nil
			}
		}
		return errors.New("no such credential")
	})
}

// Subscribe replaces a device's push subscription.
func (s Store) Subscribe(ctx context.Context, id string, sub Subscription) error {
	now := time.Now().UTC()
	sub.Created = now
	return s.with(ctx, true, func(d *database) error {
		for i := range d.Credentials {
			if d.Credentials[i].ID == id && valid(d.Credentials[i], now) {
				d.Credentials[i].Subscription = &sub
				return nil
			}
		}
		return ErrUnauthorized
	})
}

// Unsubscribe drops a device's push subscription.
func (s Store) Unsubscribe(ctx context.Context, id string) error {
	return s.with(ctx, true, func(d *database) error {
		for i := range d.Credentials {
			if d.Credentials[i].ID == id {
				d.Credentials[i].Subscription = nil
				return nil
			}
		}
		return nil
	})
}

// Subscriptions returns every subscribed device, for one push.
func (s Store) Subscriptions(ctx context.Context) ([]Entry, error) {
	var out []Entry
	now := time.Now()
	err := s.with(ctx, false, func(d *database) error {
		for _, r := range d.Credentials {
			if r.Subscription != nil && valid(r, now) {
				out = append(out, Entry{ID: r.ID, Subscription: *r.Subscription})
			}
		}
		return nil
	})
	return out, err
}

// Subscription returns a valid credential's current push subscription.
func (s Store) Subscription(ctx context.Context, id string) (Subscription, bool, error) {
	var out Subscription
	var ok bool
	now := time.Now()
	err := s.with(ctx, false, func(d *database) error {
		for _, r := range d.Credentials {
			if r.ID == id && valid(r, now) && r.Subscription != nil {
				out, ok = *r.Subscription, true
				return nil
			}
		}
		return nil
	})
	return out, ok, err
}

// Active returns the ids of every credential that is still valid.
func (s Store) Active(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	now := time.Now()
	err := s.with(ctx, false, func(d *database) error {
		for _, r := range d.Credentials {
			if valid(r, now) {
				out[r.ID] = true
			}
		}
		return nil
	})
	return out, err
}
