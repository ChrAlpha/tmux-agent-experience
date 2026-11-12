package devices

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAnInvitationPairsOneDevice(t *testing.T) {
	s := open(t)
	inv, err := s.Invite("phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Code) != 14 || strings.Count(inv.Code, "-") != 2 {
		t.Fatalf("code %q is not three groups of four", inv.Code)
	}
	// People type codes in any case and drop the separators.
	d, token, err := s.Redeem(strings.ToLower(strings.ReplaceAll(inv.Code, "-", "")), "iPhone")
	if err != nil {
		t.Fatal(err)
	}
	if d.Label != "phone" || d.Expires != nil {
		t.Fatalf("device %+v", d)
	}
	if _, _, err := s.Redeem(inv.Code, ""); !errors.Is(err, ErrInvitation) {
		t.Fatalf("a second redemption: %v", err)
	}
	got, err := s.Authenticate(token)
	if err != nil || got.ID != d.ID {
		t.Fatalf("authenticate: %+v %v", got, err)
	}
	if _, err := s.Authenticate(token + "x"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a wrong token: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(s.dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), token) || strings.Contains(string(b), normalCode(inv.Code)) {
		t.Fatal("the file holds a secret")
	}
	fi, _ := os.Stat(filepath.Join(s.dir, fileName))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", fi.Mode().Perm())
	}
}

func TestInvitationsAndCredentialsExpire(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	late, _ := s.Invite("late", 0)
	brief, _ := s.Invite("brief", time.Hour)
	now = now.Add(InvitationLife - time.Second)
	d, token, err := s.Redeem(brief.Code, "")
	if err != nil || d.Expires == nil || !d.Expires.Equal(now.Add(time.Hour)) {
		t.Fatalf("redeem: %+v %v", d, err)
	}
	now = now.Add(2 * time.Second)
	if _, _, err := s.Redeem(late.Code, ""); !errors.Is(err, ErrInvitation) {
		t.Fatalf("an expired invitation: %v", err)
	}
	now = now.Add(time.Hour)
	if _, err := s.Authenticate(token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("an expired credential: %v", err)
	}
	if list, _ := s.Devices(); len(list) != 0 {
		t.Fatalf("expired devices listed: %+v", list)
	}
}

func TestTwoDevicesCannotRedeemOneCode(t *testing.T) {
	dir := t.TempDir()
	first, _ := Open(dir)
	inv, _ := first.Invite("phone", 0)
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for range 8 {
		wg.Go(func() {
			s, _ := Open(dir) // as separate processes would
			if _, _, err := s.Redeem(inv.Code, ""); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d redemptions of one code", won)
	}
}

func TestARevocationReachesAnotherStore(t *testing.T) {
	dir := t.TempDir()
	gateway, _ := Open(dir)
	cli, _ := Open(dir)
	inv, _ := cli.Invite("phone", 0)
	d, token, err := gateway.Redeem(inv.Code, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Subscribe(d.ID, Subscription{Endpoint: "https://web.push.apple.com/a", P256DH: "k", Auth: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Authenticate(token); err != nil {
		t.Fatal(err)
	}
	listed, _ := cli.Devices()
	if len(listed) != 1 || listed[0].Push == nil {
		t.Fatalf("listed %+v", listed)
	}
	if err := cli.Revoke(d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Authenticate(token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a revoked credential: %v", err)
	}
	if err := cli.Revoke(d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking twice: %v", err)
	}
}

func TestAnOldEndpointNeverDropsANewSubscription(t *testing.T) {
	s := open(t)
	inv, _ := s.Invite("phone", 0)
	d, _, _ := s.Redeem(inv.Code, "")
	_ = s.Subscribe(d.ID, Subscription{Endpoint: "https://web.push.apple.com/old"})
	_ = s.Subscribe(d.ID, Subscription{Endpoint: "https://web.push.apple.com/new"})
	_ = s.Unsubscribe(d.ID, "https://web.push.apple.com/old")
	got, ok, _ := s.Device(d.ID)
	if !ok || got.Push == nil || got.Push.Endpoint != "https://web.push.apple.com/new" {
		t.Fatalf("device %+v", got)
	}
	_ = s.Unsubscribe(d.ID, "")
	if got, _, _ := s.Device(d.ID); got.Push != nil {
		t.Fatalf("still subscribed: %+v", got.Push)
	}
}

func TestLabelsArePrintableAndShort(t *testing.T) {
	s := open(t)
	for _, label := range []string{"a\x1b[31mb", strings.Repeat("x", maxLabel+1)} {
		if _, err := s.Invite(label, 0); err == nil {
			t.Fatalf("label %q accepted", label)
		}
	}
}

func TestADeviceNamesItselfWhenTheInvitationDoesNot(t *testing.T) {
	s := open(t)
	for named, want := range map[string]string{"iPhone · Safari": "iPhone · Safari", "": "device", "a\x1b[2Jb": "device"} {
		inv, _ := s.Invite(" ", 0)
		d, _, err := s.Redeem(inv.Code, named)
		if err != nil || d.Label != want {
			t.Fatalf("named %q: %q %v", named, d.Label, err)
		}
	}
	inv, _ := s.Invite("phone", 0)
	if d, _, _ := s.Redeem(inv.Code, "iPhone"); d.Label != "phone" {
		t.Fatalf("the invitation's label gave way to %q", d.Label)
	}
}

func TestPendingInvitationsAreBounded(t *testing.T) {
	s := open(t)
	for range maxInvitations {
		if _, err := s.Invite("phone", 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Invite("phone", 0); err == nil {
		t.Fatal("an invitation beyond the bound")
	}
}
