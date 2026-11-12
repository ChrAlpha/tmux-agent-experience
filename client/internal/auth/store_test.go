package auth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPairRedeemsOnce(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	ctx := context.Background()
	p, err := store.Pair(ctx, "phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Code, "-") {
		t.Fatalf("code %q is not grouped", p.Code)
	}
	issued, err := store.Redeem(ctx, strings.ToLower(strings.ReplaceAll(p.Code, "-", "")))
	if err != nil || issued.Label != "phone" || issued.Token == "" {
		t.Fatalf("redeem: %+v %v", issued, err)
	}
	if _, err := store.Redeem(ctx, p.Code); !errors.Is(err, ErrInvitation) {
		t.Fatalf("a code redeemed twice: %v", err)
	}
	if _, list, err := store.List(ctx); err != nil || len(list) != 1 || list[0].ID != issued.ID {
		t.Fatalf("list: %+v %v", list, err)
	}
	got, err := store.Authenticate(ctx, issued.Token)
	if err != nil || got.ID != issued.ID || got.Push {
		t.Fatalf("authenticate: %+v %v", got, err)
	}
	// The file holds a digest, never the token.
	b, err := os.ReadFile(filepath.Join(store.Dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), issued.Token) {
		t.Fatal("the credential token is in the store")
	}
	if st, _ := os.Stat(filepath.Join(store.Dir, "credentials.json")); st.Mode().Perm() != 0600 {
		t.Fatalf("credentials file mode %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(store.Dir); st.Mode().Perm() != 0700 {
		t.Fatalf("state directory mode %v", st.Mode().Perm())
	}
	if err := store.Revoke(ctx, issued.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(ctx, issued.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a revoked credential authenticated: %v", err)
	}
}

func TestRedeemNamesTheInvitation(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	ctx := context.Background()
	if _, err := store.Redeem(ctx, "0000-0000-0000"); !errors.Is(err, ErrInvitation) || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("an unknown code: %v", err)
	} else if !strings.Contains(err.Error(), "code is invalid, expired or already used") {
		t.Fatalf("an unknown code says %q", err)
	}
	p, err := store.Pair(ctx, "phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := store.Redeem(ctx, p.Code)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Redeem(ctx, p.Code); !errors.Is(err, ErrInvitation) || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a redeemed code: %v", err)
	}
	if _, err := store.Authenticate(ctx, issued.Token[:len(issued.Token)-1]+"z"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a bad credential: %v", err)
	}
}

func TestInvitationExpires(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	ctx := context.Background()
	p, err := store.Pair(ctx, "phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, "credentials.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d database
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	d.Invitations[0].Expires = time.Now().Add(-time.Minute)
	b, _ = json.Marshal(d)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Redeem(ctx, p.Code); !errors.Is(err, ErrInvitation) {
		t.Fatalf("an expired code: %v", err)
	}
}

func TestConcurrentRedemption(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	ctx := context.Background()
	p, err := store.Pair(ctx, "phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	const racers = 24
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.Redeem(ctx, p.Code); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			} else if !errors.Is(err, ErrInvitation) {
				t.Errorf("redeem: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d devices redeemed one code", wins)
	}
}

func TestSubscriptionsFollowCredentials(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	ctx := context.Background()
	p, err := store.Pair(ctx, "phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := store.Redeem(ctx, p.Code)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Subscribe(ctx, issued.ID, Subscription{Endpoint: "https://push.example.com/x", P256DH: "a", Auth: "b"}); err != nil {
		t.Fatal(err)
	}
	_, list, err := store.List(ctx)
	if err != nil || len(list) != 1 || !list[0].Push {
		t.Fatalf("list after subscribe: %+v %v", list, err)
	}
	subs, err := store.Subscriptions(ctx)
	if err != nil || len(subs) != 1 || subs[0].ID != issued.ID || subs[0].Subscription.Endpoint != "https://push.example.com/x" {
		t.Fatalf("subscriptions: %+v %v", subs, err)
	}
	if err := store.Revoke(ctx, issued.ID); err != nil {
		t.Fatal(err)
	}
	subs, err = store.Subscriptions(ctx)
	if err != nil || len(subs) != 0 {
		t.Fatalf("revocation kept a subscription: %+v %v", subs, err)
	}
}

func TestCredentialLifetime(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	ctx := context.Background()
	if _, err := store.Pair(ctx, "phone", -time.Second); err == nil {
		t.Fatal("a negative lifetime was accepted")
	}
	p, err := store.Pair(ctx, "phone", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := store.Redeem(ctx, p.Code)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := store.Authenticate(ctx, issued.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("an expired credential authenticated: %v", err)
	}
}
