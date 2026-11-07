package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCredentialsPersistAndRevokeIndependently(t *testing.T) {
	ctx := context.Background()
	store := Store{Dir: t.TempDir()}
	a, err := store.Issue(ctx, "desktop", 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Issue(ctx, "script", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(store.Dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), a.Token) || strings.Contains(string(data), b.Token) {
		t.Fatal("stored a bearer secret")
	}
	info, _ := os.Stat(filepath.Join(store.Dir, "credentials.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("authorization database permissions")
	}
	reopened := Store{Dir: store.Dir}
	g, err := reopened.Authenticate(ctx, a.Token)
	if err != nil || g.ID() != a.ID {
		t.Fatal("credential did not survive reopening")
	}
	instance, list, err := reopened.List(ctx)
	if err != nil || instance == "" || len(list) != 2 {
		t.Fatal("missing instance or credentials")
	}
	if err = store.Revoke(ctx, a.ID, false); err != nil {
		t.Fatal(err)
	}
	if reopened.Active(ctx, g) {
		t.Fatal("revoked grant remains active")
	}
	if _, err = reopened.Authenticate(ctx, a.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("revoked token admitted")
	}
	if _, err = reopened.Authenticate(ctx, b.Token); err != nil {
		t.Fatal("revoking another client revoked this one")
	}
	if _, err = (Store{Dir: t.TempDir()}).Authenticate(ctx, b.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("credential crosses instances")
	}
	if _, err = store.Authenticate(ctx, b.Token[:len(b.Token)-1]+"z"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("forged token admitted")
	}
}
func TestPairingIsSingleUseAcrossProcesses(t *testing.T) {
	ctx := context.Background()
	s := Store{Dir: t.TempDir()}
	p, err := s.Pair(ctx, "phone", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(s.Dir, "credentials.json"))
	if strings.Contains(string(data), p.Code) {
		t.Fatal("stored pairing secret")
	}
	var wg sync.WaitGroup
	outcomes := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := (Store{Dir: s.Dir}).Redeem(ctx, p.Code); outcomes <- err }()
	}
	wg.Wait()
	close(outcomes)
	successes := 0
	for err := range outcomes {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrUnauthorized) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("one invitation issued %d credentials", successes)
	}
	_, list, _ := s.List(ctx)
	if len(list) != 1 || list[0].Expires == nil {
		t.Fatal("pairing lost the credential lifetime")
	}
}
func TestExpiryAndRevokeAll(t *testing.T) {
	ctx := context.Background()
	s := Store{Dir: t.TempDir()}
	c, err := s.Issue(ctx, "expired", time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Authenticate(ctx, c.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("expired credential admitted")
	}
	p, err := s.Pair(ctx, "pending", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Revoke(ctx, "", true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Redeem(ctx, p.Code); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("revoke all left an invitation active")
	}
}
func TestAuthorizationDirectoryIsIndependentOfRealm(t *testing.T) {
	t.Setenv("TMAG_AUTH_DIR", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	before := DefaultDir()
	t.Setenv("TMAG_STATE_DIR", t.TempDir())
	if DefaultDir() != before {
		t.Fatal("session directory changed authorization scope")
	}
	override := t.TempDir()
	t.Setenv("TMAG_AUTH_DIR", override)
	if DefaultDir() != override {
		t.Fatal("explicit instance directory ignored")
	}
}
