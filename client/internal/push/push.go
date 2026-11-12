// Package push sends Web Push messages: VAPID (RFC 8292) and aes128gcm
// content encryption (RFC 8291, RFC 8188).
package push

import (
	"bytes"
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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Keys are the gateway's VAPID signing keys. The pair is generated once and
// kept in the state directory.
type Keys struct {
	private *ecdsa.PrivateKey
	public  []byte // uncompressed P-256 point
}

type keyFile struct {
	Private string `json:"private"`
	Public  string `json:"public"`
}

// LoadKeys reads the VAPID pair from dir, generating it once when absent.
func LoadKeys(dir string) (*Keys, error) {
	path := filepath.Join(dir, "vapid.json")
	b, err := os.ReadFile(path)
	if err == nil {
		var f keyFile
		if json.Unmarshal(b, &f) != nil {
			return nil, errors.New("invalid VAPID key file")
		}
		priv, derr := base64.RawURLEncoding.DecodeString(f.Private)
		pub, perr := base64.RawURLEncoding.DecodeString(f.Public)
		if derr != nil || perr != nil || len(priv) != 32 || len(pub) != 65 {
			return nil, errors.New("invalid VAPID key file")
		}
		key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), priv)
		if err != nil {
			return nil, err
		}
		return &Keys{private: key, public: pub}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	pub, err := key.PublicKey.Bytes()
	if err != nil {
		return nil, err
	}
	priv, err := key.Bytes()
	if err != nil {
		return nil, err
	}
	f := keyFile{
		Private: base64.RawURLEncoding.EncodeToString(priv),
		Public:  base64.RawURLEncoding.EncodeToString(pub),
	}
	b, _ = json.MarshalIndent(f, "", "  ")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	fh, err := os.CreateTemp(dir, ".vapid-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(fh.Name())
	_, err = fh.Write(append(b, '\n'))
	if err == nil {
		err = fh.Sync()
	}
	err = errors.Join(err, fh.Close())
	if err != nil {
		return nil, err
	}
	if err := os.Rename(fh.Name(), path); err != nil {
		return nil, err
	}
	return &Keys{private: key, public: pub}, nil
}

// Public is the application server key a browser subscribes with.
func (k *Keys) Public() string { return base64.RawURLEncoding.EncodeToString(k.public) }

// VAPID returns the authorization header value for an endpoint's origin.
func (k *Keys) VAPID(audience, subject string, now time.Time, life time.Duration) (string, error) {
	if life <= 0 || life > 24*time.Hour {
		return "", errors.New("a VAPID token lives less than a day")
	}
	header := map[string]string{"typ": "JWT", "alg": "ES256"}
	claims := map[string]any{"aud": audience, "exp": now.Add(life).Unix(), "sub": subject}
	part := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signed := part(header) + "." + part(claims)
	sum := sha256.Sum256([]byte(signed))
	r, s, err := ecdsa.Sign(rand.Reader, k.private, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "vapid t=" + signed + "." + base64.RawURLEncoding.EncodeToString(sig) + ", k=" + k.Public(), nil
}

// Subscription is a browser's push subscription.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	P256DH   string `json:"p256dh"`
	Auth     string `json:"auth"`
}

// ErrGone is a subscription the push service no longer has.
var ErrGone = errors.New("push subscription is gone")

// nonPublic are the address ranges a push endpoint must never name, beyond
// what net.IP already reports.
var nonPublic = func() []*net.IPNet {
	// The special-purpose registries: shared, protocol, documentation,
	// benchmarking, reserved and multicast IPv4, and IPv6 unique-local,
	// documentation, NAT64 and other non-global forms.
	cidrs := []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
		"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
		"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23",
		"2001:db8::/32", "2002::/16", "3fff::/20", "fc00::/7", "fe80::/10", "ff00::/8",
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

// publicAddr reports whether an address is globally routable.
func publicAddr(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range nonPublic {
		if n.Contains(ip) {
			return false
		}
	}
	return ip.IsGlobalUnicast()
}

// ValidateEndpoint accepts only HTTPS endpoints whose host is public.
func ValidateEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("a push endpoint must be an HTTPS URL without userinfo or fragment")
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return nil, errors.New("a push endpoint must not be local")
	}
	if ip := net.ParseIP(host); ip != nil && !publicAddr(ip) {
		return nil, errors.New("a push endpoint must be globally routable, not loopback, private, link-local or otherwise reserved")
	}
	return u, nil
}

// Validate checks a subscription with this client's endpoint check.
func (c *Client) Validate(s Subscription) (*url.URL, error) {
	check := c.Endpoint
	if check == nil {
		check = ValidateEndpoint
	}
	u, err := check(s.Endpoint)
	if err != nil {
		return nil, err
	}
	if err := validateKeys(s); err != nil {
		return nil, err
	}
	return u, nil
}

// Validate checks a subscription and its endpoint.
func Validate(s Subscription) (*url.URL, error) { return (&Client{}).Validate(s) }

func validateKeys(s Subscription) error {
	key, err := base64.RawURLEncoding.DecodeString(s.P256DH)
	if err != nil || len(key) != 65 {
		return errors.New("a push subscription needs a P-256 public key")
	}
	if _, err := ecdh.P256().NewPublicKey(key); err != nil {
		return errors.New("a push subscription needs a valid P-256 public key")
	}
	auth, err := base64.RawURLEncoding.DecodeString(s.Auth)
	if err != nil || len(auth) < 16 {
		return errors.New("a push subscription needs a 16-byte auth secret")
	}
	return nil
}

// Message is what a notification says and how it behaves. The metadata
// fields name the machine and session; a payload never holds a prompt, a
// reply or a tool's input.
type Message struct {
	Title   string `json:"title"`
	Body    string `json:"body"`
	Tag     string `json:"tag,omitempty"`
	URL     string `json:"url,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Machine string `json:"machine,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Session string `json:"session,omitempty"`
	Request string `json:"request,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Dir     string `json:"dir,omitempty"`

	Urgency string `json:"-"`
	TTL     int    `json:"-"`
	Topic   string `json:"-"`
}

// Client sends messages to push services.
type Client struct {
	// HTTP overrides the push client for tests. Nil uses the guarded
	// transport below.
	HTTP    *http.Client
	Keys    *Keys
	Subject string
	Now     func() time.Time

	// Endpoint checks a subscription's endpoint. Tests reach a local push
	// service with it; nil means ValidateEndpoint.
	Endpoint func(string) (*url.URL, error)
}

// Send encrypts msg for one subscription and delivers it.
func (c *Client) Send(ctx context.Context, s Subscription, msg Message) error {
	check := c.Endpoint
	if check == nil {
		check = ValidateEndpoint
	}
	u, err := check(s.Endpoint)
	if err != nil {
		return err
	}
	if err := validateKeys(s); err != nil {
		return err
	}
	body, err := encrypt(s, msg)
	if err != nil {
		return err
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	subject := c.Subject
	if subject == "" {
		return errors.New("push needs a public origin for VAPID")
	}
	auth, err := c.Keys.VAPID(u.Scheme+"://"+u.Host, subject, now(), 12*time.Hour)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", fmt.Sprint(msg.TTL))
	if msg.Urgency != "" {
		req.Header.Set("Urgency", msg.Urgency)
	}
	if msg.Topic != "" {
		req.Header.Set("Topic", msg.Topic)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{
			Timeout:       10 * time.Second,
			Transport:     guardedTransport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	} else {
		copied := *client
		if copied.Transport == nil {
			copied.Transport = guardedTransport
		}
		copied.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &copied
	}
	res, err := client.Do(req)
	if err != nil {
		var failed *url.Error
		if errors.As(err, &failed) {
			return fmt.Errorf("push request failed: %w", failed.Err)
		}
		return fmt.Errorf("push request failed: %w", err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
	switch {
	case res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusGone:
		return ErrGone
	case res.StatusCode/100 != 2:
		return fmt.Errorf("push service answered %s", res.Status)
	}
	return nil
}

// guardedTransport dials a push service directly, never a proxy, and refuses
// an address a name resolved to that must stay on the machine or its network.
var guardedTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if ok {
		t = t.Clone()
	} else {
		t = &http.Transport{}
	}
	t.Proxy = nil
	t.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   dialControl,
	}).DialContext
	return t
}()

// dialControl refuses the addresses a push endpoint must never reach.
func dialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if i := strings.LastIndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return errors.New("push endpoint address is not an IP")
	}
	if !publicAddr(ip) {
		return errors.New("a push endpoint must be globally routable, not loopback, private, link-local or otherwise reserved")
	}
	return nil
}

// encrypt builds one aes128gcm record, as RFC 8188 and RFC 8291 define it.
func encrypt(s Subscription, msg Message) ([]byte, error) {
	uaBytes, _ := base64.RawURLEncoding.DecodeString(s.P256DH)
	authSecret, _ := base64.RawURLEncoding.DecodeString(s.Auth)
	ua, err := ecdh.P256().NewPublicKey(uaBytes)
	if err != nil {
		return nil, err
	}
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := as.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asBytes := as.PublicKey().Bytes()
	info := append([]byte("WebPush: info\x00"), uaBytes...)
	info = append(info, asBytes...)
	prk, err := hkdf.Extract(sha256.New, shared, authSecret)
	if err != nil {
		return nil, err
	}
	ikm, err := hkdf.Expand(sha256.New, prk, string(info), 32)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	prk2, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk2, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk2, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(msg)
	plain := make([]byte, len(payload)+1)
	copy(plain, payload)
	plain[len(payload)] = 0x02
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nil, nonce, plain, nil)
	const recordSize = 4096
	header := make([]byte, 0, 16+4+1+len(asBytes))
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint32(header, recordSize)
	header = append(header, byte(len(asBytes)))
	header = append(header, asBytes...)
	return append(header, sealed...), nil
}
