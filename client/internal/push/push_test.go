package push

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func keys(t *testing.T) *Keys {
	t.Helper()
	k, err := LoadKeys(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestVAPIDAndEncryption(t *testing.T) {
	vapid := keys(t)
	ua, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authSecret := make([]byte, 16)
	_, _ = rand.Read(authSecret)
	sub := Subscription{
		Endpoint: "https://push.example.com/send/abc",
		P256DH:   base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes()),
		Auth:     base64.RawURLEncoding.EncodeToString(authSecret),
	}
	now := time.Unix(1_800_000_000, 0)
	msg := Message{Title: "claude finished", Body: "go · box", Kind: "finished", Machine: "box", Agent: "claude", Session: "s1", Tag: "tmag-x", URL: "#/s/s1", Urgency: "normal", TTL: 60, Topic: "tmag-x"}
	var got *http.Request
	var body []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	sub.Endpoint = server.URL + "/send/abc"
	c := &Client{HTTP: server.Client(), Keys: vapid, Subject: "https://tmag.example.com", Now: func() time.Time { return now }, Endpoint: url.Parse}
	if err := c.Send(context.Background(), sub, msg); err != nil {
		t.Fatal(err)
	}
	if got.Header.Get("Content-Encoding") != "aes128gcm" || got.Header.Get("TTL") != "60" || got.Header.Get("Urgency") != "normal" || got.Header.Get("Topic") != "tmag-x" {
		t.Fatalf("headers: %v", got.Header)
	}
	verifyVAPID(t, got.Header.Get("Authorization"), server.URL, "https://tmag.example.com", now, vapid)
	plain := decrypt(t, body, ua, authSecret)
	var payload Message
	if err := json.Unmarshal(plain, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Title != msg.Title || payload.Machine != "box" || payload.Session != "s1" || payload.Tool != "" {
		t.Fatalf("payload: %+v", payload)
	}
}

func verifyVAPID(t *testing.T, header, endpoint, subject string, now time.Time, keys *Keys) {
	t.Helper()
	value, ok := strings.CutPrefix(header, "vapid ")
	if !ok {
		t.Fatalf("authorization %q", header)
	}
	parts := strings.Split(value, ", ")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "t=") || !strings.HasPrefix(parts[1], "k=") {
		t.Fatalf("authorization %q", header)
	}
	token := strings.TrimPrefix(parts[0], "t=")
	if strings.TrimPrefix(parts[1], "k=") != keys.Public() {
		t.Fatal("the wrong application server key")
	}
	seg := strings.Split(token, ".")
	if len(seg) != 3 {
		t.Fatalf("token %q", token)
	}
	var h struct{ Alg, Typ string }
	decode(t, seg[0], &h)
	if h.Alg != "ES256" {
		t.Fatalf("alg %q", h.Alg)
	}
	var c struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	decode(t, seg[1], &c)
	u, _ := url.Parse(endpoint)
	if c.Aud != u.Scheme+"://"+u.Host || c.Sub != subject || c.Exp <= now.Unix() || c.Exp > now.Add(24*time.Hour).Unix() {
		t.Fatalf("claims: %+v", c)
	}
	pub, _ := base64.RawURLEncoding.DecodeString(keys.Public())
	key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), pub)
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(seg[2])
	if len(sig) != 64 {
		t.Fatalf("signature length %d", len(sig))
	}
	sum := sha256.Sum256([]byte(seg[0] + "." + seg[1]))
	if !ecdsa.Verify(key, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("the VAPID signature does not verify")
	}
}

func decode(t *testing.T, part string, v any) {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil || json.Unmarshal(b, v) != nil {
		t.Fatalf("part %q: %v", part, err)
	}
}

func decrypt(t *testing.T, body []byte, ua *ecdh.PrivateKey, authSecret []byte) []byte {
	t.Helper()
	if len(body) < 16+4+1+65 {
		t.Fatalf("body is %d bytes", len(body))
	}
	salt := body[:16]
	rs := binary.BigEndian.Uint32(body[16:20])
	idlen := int(body[20])
	asBytes := body[21 : 21+idlen]
	ciphertext := body[21+idlen:]
	if rs != 4096 || idlen != 65 {
		t.Fatalf("record header rs=%d idlen=%d", rs, idlen)
	}
	as, err := ecdh.P256().NewPublicKey(asBytes)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := ua.ECDH(as)
	if err != nil {
		t.Fatal(err)
	}
	info := append([]byte("WebPush: info\x00"), ua.PublicKey().Bytes()...)
	info = append(info, asBytes...)
	prk, _ := hkdf.Extract(sha256.New, shared, authSecret)
	ikm, _ := hkdf.Expand(sha256.New, prk, string(info), 32)
	prk2, _ := hkdf.Extract(sha256.New, ikm, salt)
	cek, _ := hkdf.Expand(sha256.New, prk2, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Expand(sha256.New, prk2, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) == 0 || plain[len(plain)-1] != 0x02 {
		t.Fatalf("no last-record delimiter: %q", plain)
	}
	return plain[:len(plain)-1]
}

func TestGone(t *testing.T) {
	vapid := keys(t)
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	authSecret := make([]byte, 16)
	_, _ = rand.Read(authSecret)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer server.Close()
	c := &Client{HTTP: server.Client(), Keys: vapid, Subject: "https://tmag.example.com", Endpoint: url.Parse}
	err := c.Send(context.Background(), Subscription{
		Endpoint: server.URL + "/push",
		P256DH:   base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes()),
		Auth:     base64.RawURLEncoding.EncodeToString(authSecret),
	}, Message{Title: "t"})
	if !errors.Is(err, ErrGone) {
		t.Fatalf("410: %v", err)
	}
}

func TestDialControlRefusesRestrictedAddresses(t *testing.T) {
	for _, address := range []string{
		"127.0.0.1:443", "10.0.0.1:443", "192.168.1.2:443", "172.16.0.1:443",
		"169.254.169.254:443", "0.0.0.0:443", "224.0.0.1:443",
		"0.1.2.3:443", "100.64.0.1:443", "100.127.255.254:443", "192.0.0.1:443",
		"192.0.2.1:443", "192.88.99.1:443", "198.18.0.1:443", "198.19.255.255:443",
		"198.51.100.1:443", "203.0.113.1:443", "240.0.0.1:443", "255.255.255.255:443",
		"[::1]:443", "[::]:443", "[ffff::1]:443", "[fe80::1]:443", "[ff02::1]:443",
		"[::ffff:127.0.0.1]:443", "[::ffff:10.0.0.1]:443", "[::ffff:100.64.0.1]:443",
		"[64:ff9b::1]:443", "[64:ff9b:1::1]:443", "[100::1]:443", "[2001::1]:443",
		"[2001:db8::1]:443", "[2002::1]:443", "[3fff::1]:443", "[fc00::1]:443", "[fd00::1]:443",
	} {
		if err := dialControl("tcp", address, nil); err == nil {
			t.Errorf("%s was not refused", address)
		}
	}
	for _, address := range []string{"8.8.8.8:443", "1.1.1.1:443", "[2606:4700:4700::1111]:443"} {
		if err := dialControl("tcp", address, nil); err != nil {
			t.Fatalf("a public address %s: %v", address, err)
		}
	}
}

func TestSendRefusesLocalAtDial(t *testing.T) {
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	authSecret := make([]byte, 16)
	_, _ = rand.Read(authSecret)
	c := &Client{Keys: keys(t), Subject: "https://tmag.example.com", Endpoint: url.Parse}
	err := c.Send(context.Background(), Subscription{
		Endpoint: "https://127.0.0.1:1/push",
		P256DH:   base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes()),
		Auth:     base64.RawURLEncoding.EncodeToString(authSecret),
	}, Message{Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("local push endpoint: %v", err)
	}
}

func TestValidateEndpoint(t *testing.T) {
	for _, bad := range []string{
		"http://push.example.com/x", "https://localhost/x", "https://127.0.0.1/x", "https://[::1]/x",
		"https://10.0.0.1/x", "https://192.168.1.2/x", "https://169.254.1.1/x", "https://0.0.0.0/x",
		"https://0.1.2.3/x", "https://100.64.0.1/x", "https://192.0.0.1/x", "https://192.0.2.1/x",
		"https://198.18.0.1/x", "https://198.51.100.1/x", "https://203.0.113.1/x",
		"https://240.0.0.1/x", "https://255.255.255.255/x",
		"https://[64:ff9b::1]/x", "https://[64:ff9b:1::1]/x", "https://[2001:db8::1]/x",
		"https://[3fff::1]/x", "https://[fc00::1]/x", "https://[fd00::1]/x", "https://[fe80::1]/x",
		"https://user:pass@push.example.com/x", "https://push.example.com/x#fragment",
	} {
		if _, err := ValidateEndpoint(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := ValidateEndpoint("https://push.example.com/send/abc"); err != nil {
		t.Fatalf("a public endpoint: %v", err)
	}
}

func TestKeysPersist(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadKeys(dir)
	if err != nil || a.Public() != b.Public() {
		t.Fatalf("keys changed: %q %q %v", a.Public(), b.Public(), err)
	}
	if _, err := a.VAPID("https://box", "https://tmag.example.com", time.Now(), 25*time.Hour); err == nil {
		t.Fatal("a VAPID token lived more than a day")
	}
}
