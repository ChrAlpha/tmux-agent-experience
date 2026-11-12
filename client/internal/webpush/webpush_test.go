package webpush

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/client/internal/webpush/webpushtest"
)

var b64 = base64.RawURLEncoding

func mustDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := b64.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// RFC 8291 section 5 and appendix A: the same inputs give the same message.
func TestEncryptionMatchesRFC8291(t *testing.T) {
	as, err := ecdh.P256().NewPrivateKey(mustDecode(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	sub := Subscription{
		P256DH: "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4",
		Auth:   "BTBZMqHH6r4Tts7J_aSIgg",
	}
	got, err := encrypt(sub, []byte("When I grow up, I want to be a watermelon"), as, mustDecode(t, "DGv6ra1nlYgDCS1FRnbzlw"))
	if err != nil {
		t.Fatal(err)
	}
	want := `DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27ml
	mlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPT
	pK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN`
	if b64.EncodeToString(got) != strings.Join(strings.Fields(want), "") {
		t.Fatalf("message\n%s\nwant\n%s", b64.EncodeToString(got), want)
	}
}

func newBrowser(t *testing.T, endpoint string) (*webpushtest.Receiver, Subscription) {
	t.Helper()
	r, p256dh, auth := webpushtest.New()
	return r, Subscription{Endpoint: endpoint, P256DH: p256dh, Auth: auth}
}

// verifyVAPID checks the Authorization header as a push service does.
func verifyVAPID(t *testing.T, header, origin string) map[string]any {
	t.Helper()
	rest, ok := strings.CutPrefix(header, "vapid t=")
	if !ok {
		t.Fatalf("authorization %q", header)
	}
	token, key, ok := strings.Cut(rest, ", k=")
	if !ok {
		t.Fatalf("authorization %q", header)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token %q", token)
	}
	var head map[string]any
	_ = json.Unmarshal(mustDecode(t, parts[0]), &head)
	if head["alg"] != "ES256" || head["typ"] != "JWT" {
		t.Fatalf("header %v", head)
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), mustDecode(t, key))
	if err != nil {
		t.Fatal(err)
	}
	sig := mustDecode(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature of %d bytes", len(sig))
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("the signature does not verify")
	}
	var claims map[string]any
	_ = json.Unmarshal(mustDecode(t, parts[1]), &claims)
	if claims["aud"] != origin {
		t.Fatalf("aud %v, want %s", claims["aud"], origin)
	}
	exp := time.Unix(int64(claims["exp"].(float64)), 0)
	if d := time.Until(exp); d <= 0 || d > 24*time.Hour {
		t.Fatalf("exp in %s", d)
	}
	return claims
}

func TestAPushServiceReadsWhatWasSent(t *testing.T) {
	keys, err := LoadKeys(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var got *http.Request
	var body []byte
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, body = r, must(io.ReadAll(r.Body))
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	b, sub := newBrowser(t, srv.URL+"/push/abc")
	c := &Client{Keys: keys, Subject: "https://box.example.ts.net", Extra: []string{strings.TrimPrefix(srv.URL, "https://")}, HTTP: srv.Client()}
	err = c.Send(context.Background(), sub, Message{Payload: []byte(`{"kind":"blocked"}`), TTL: time.Hour, Urgency: "high", Topic: "0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := b.Open(body); err != nil || string(got) != `{"kind":"blocked"}` {
		t.Fatalf("the browser read %q, %v", got, err)
	}
	for k, v := range map[string]string{"Content-Encoding": "aes128gcm", "TTL": "3600", "Urgency": "high", "Topic": "0123456789abcdef0123456789abcdef"} {
		if got.Header.Get(k) != v {
			t.Fatalf("%s = %q, want %q", k, got.Header.Get(k), v)
		}
	}
	claims := verifyVAPID(t, got.Header.Get("Authorization"), srv.URL)
	if claims["sub"] != "https://box.example.ts.net" {
		t.Fatalf("sub %v", claims["sub"])
	}
}

func TestRefusalsSayWhy(t *testing.T) {
	keys, _ := LoadKeys(t.TempDir())
	status, answer := http.StatusGone, ""
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	defer srv.Close()
	_, sub := newBrowser(t, srv.URL+"/push")
	c := &Client{Keys: keys, Subject: "mailto:me@example.com", Extra: []string{strings.TrimPrefix(srv.URL, "https://")}, HTTP: srv.Client()}
	send := func() error {
		return c.Send(context.Background(), sub, Message{Payload: []byte("{}"), TTL: time.Minute})
	}
	if err := send(); !errors.Is(err, ErrGone) {
		t.Fatalf("410: %v", err)
	}
	status, answer = http.StatusBadRequest, `{"reason":"BadWebPushTopic"}`
	if err := send(); err == nil || !strings.HasSuffix(err.Error(), ": BadWebPushTopic") {
		t.Fatalf("400: %v", err)
	}
	status, answer = http.StatusForbidden, "denied\n\x1b[2Jfor\tthis key"
	if err := send(); err == nil || !strings.HasSuffix(err.Error(), ": denied [2Jfor this key") {
		t.Fatalf("403: %v", err)
	}
}

func TestOnlyPushServicesTakeMessages(t *testing.T) {
	c := &Client{Extra: []string{"push.example.net", "localhost:8443"}}
	for _, ok := range []string{
		"https://fcm.googleapis.com/fcm/send/x",
		"https://updates.push.services.mozilla.com/wpush/v2/x",
		"https://web.push.apple.com/QH0x",
		"https://wns2-par02p.notify.windows.com/w/?token=x",
		"https://WEB.PUSH.APPLE.COM:443/x",
		"https://push.example.net/x",
		"https://localhost:8443/x",
	} {
		if err := c.Allowed(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://fcm.googleapis.com/fcm/send/x",
		"https://fcm.googleapis.com:8080/x",
		"https://fcm.googleapis.com.evil.example/x",
		"https://evilfcm.googleapis.com/x",
		"https://notify.windows.com/x",
		"https://xnotify.windows.com/x",
		"https://user@web.push.apple.com/x",
		"https://localhost/x",
		"https://127.0.0.1/x",
		"https://example.com/push",
		"not a url",
	} {
		if err := c.Allowed(bad); err == nil {
			t.Errorf("%s allowed", bad)
		}
	}
}

func TestKeysLastAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadKeys(dir)
	if err != nil || a.Public() != b.Public() {
		t.Fatalf("keys changed: %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "vapid.json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if len(mustDecode(t, a.Public())) != 65 {
		t.Fatal("public key is not an uncompressed point")
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
