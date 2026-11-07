// Package auth manages one owner's independently revocable client credentials.
// Its persistent directory is independent of the process realm's session state.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
)

var ErrUnauthorized = errors.New("credential is invalid, expired or revoked")

// DefaultDir persists across boots. Deployments sharing a home can select
// separate authorization stores with TMAG_AUTH_DIR, independently of state.
func DefaultDir() string {
	if d := os.Getenv("TMAG_AUTH_DIR"); d != "" {
		return d
	}
	return filepath.Join(eventlog.Base(), "auth")
}

type Store struct{ Dir string }
type Credential struct {
	ID      string     `json:"id"`
	Label   string     `json:"label"`
	Created time.Time  `json:"created"`
	Expires *time.Time `json:"expires,omitempty"`
}
type record struct {
	Credential
	Digest string `json:"digest"`
}
type invitation struct {
	Digest  string        `json:"digest"`
	Label   string        `json:"label"`
	Expires time.Time     `json:"expires"`
	TTL     time.Duration `json:"ttl"`
}
type database struct {
	Version     int         `json:"version"`
	Instance    string      `json:"instance"`
	Credentials []record    `json:"credentials"`
	Pair        *invitation `json:"pair,omitempty"`
}

// Grant can only be obtained by validating a credential. Its zero value conveys
// no authority. Internal callers carry it to the query layer without a public
// command-line flag or environment variable that could impersonate the owner.
type Grant struct{ id string }

func (g Grant) ID() string { return g.id }

type Issued struct {
	Credential
	Token string `json:"token"`
}
type Invitation struct {
	Code    string    `json:"code"`
	Expires time.Time `json:"expires"`
}

func random(n int) string    { b := make([]byte, n); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// with serializes CLI management and HTTP admissions across processes. Only
// verification digests are persisted; a crash leaves either complete database.
func (s Store) with(ctx context.Context, write bool, fn func(*database) error) error {
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return err
	}
	return lockfile.With(ctx, filepath.Join(s.Dir, "lock"), func() error {
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
			if err = json.Unmarshal(b, &d); err != nil {
				return fmt.Errorf("read authorization store: %w", err)
			}
			if d.Version != 1 || d.Instance == "" {
				return errors.New("unsupported authorization store")
			}
		}
		if err = fn(&d); err != nil {
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
	})
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
func issue(d *database, label string, ttl time.Duration) Issued {
	now := time.Now().UTC()
	c := Credential{ID: random(8), Label: label, Created: now}
	if ttl > 0 {
		t := now.Add(ttl)
		c.Expires = &t
	}
	token := c.ID + "." + random(32)
	d.Credentials = append(d.Credentials, record{Credential: c, Digest: digest(token)})
	return Issued{Credential: c, Token: token}
}
func (s Store) Issue(ctx context.Context, label string, ttl time.Duration) (Issued, error) {
	var out Issued
	if err := labelOK(label, ttl); err != nil {
		return out, err
	}
	err := s.with(ctx, true, func(d *database) error { out = issue(d, label, ttl); return nil })
	return out, err
}
func (s Store) List(ctx context.Context) (string, []Credential, error) {
	var instance string
	out := []Credential{}
	err := s.with(ctx, false, func(d *database) error {
		instance = d.Instance
		for _, r := range d.Credentials {
			out = append(out, r.Credential)
		}
		return nil
	})
	return instance, out, err
}
func (s Store) Authenticate(ctx context.Context, token string) (Grant, error) {
	var g Grant
	if len(token) != 81 || token[16] != '.' {
		return g, ErrUnauthorized
	}
	err := s.with(ctx, false, func(d *database) error {
		for _, r := range d.Credentials {
			if r.ID == token[:16] && valid(r, time.Now()) && equal(r.Digest, digest(token)) {
				g.id = r.ID
				return nil
			}
		}
		return ErrUnauthorized
	})
	return g, err
}

// Validate checks whether a previously authenticated grant is still admitted.
func (s Store) Validate(ctx context.Context, g Grant) error {
	if g.id == "" {
		return ErrUnauthorized
	}
	return s.with(ctx, false, func(d *database) error {
		for _, r := range d.Credentials {
			if r.ID == g.id && valid(r, time.Now()) {
				return nil
			}
		}
		return ErrUnauthorized
	})
}
func (s Store) Active(ctx context.Context, g Grant) bool { return s.Validate(ctx, g) == nil }
func (s Store) Revoke(ctx context.Context, id string, all bool) error {
	return s.with(ctx, true, func(d *database) error {
		if all {
			d.Credentials = []record{}
			d.Pair = nil
			return nil
		}
		for i, r := range d.Credentials {
			if r.ID == id {
				d.Credentials = append(d.Credentials[:i], d.Credentials[i+1:]...)
				return nil
			}
		}
		return errors.New("no such credential")
	})
}
func (s Store) Pair(ctx context.Context, label string, ttl time.Duration) (Invitation, error) {
	var out Invitation
	if err := labelOK(label, ttl); err != nil {
		return out, err
	}
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	out = Invitation{Code: base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), Expires: time.Now().UTC().Add(10 * time.Minute)}
	err := s.with(ctx, true, func(d *database) error {
		d.Pair = &invitation{Digest: digest(out.Code), Label: label, Expires: out.Expires, TTL: ttl}
		return nil
	})
	return out, err
}
func (s Store) Redeem(ctx context.Context, code string) (Issued, error) {
	var out Issued
	code = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(code))
	err := s.with(ctx, true, func(d *database) error {
		p := d.Pair
		if p == nil || !time.Now().Before(p.Expires) || !equal(p.Digest, digest(code)) {
			return ErrUnauthorized
		}
		out = issue(d, p.Label, p.TTL)
		d.Pair = nil
		return nil
	})
	return out, err
}
