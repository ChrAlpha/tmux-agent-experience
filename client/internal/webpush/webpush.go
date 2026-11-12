// Package webpush sends Web Push messages: encrypted for one browser's
// subscription with aes128gcm (RFC 8291, RFC 8188) and signed for the push
// service with VAPID (RFC 8292). A message goes only to a known push
// service, so a subscription can never make the gateway call another host.
package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ErrGone is a push service saying the subscription no longer exists.
var ErrGone = errors.New("the push service no longer knows this subscription")

// Services are the push services of the browsers that support Web Push.
// Edge's live under notify.windows.com.
var Services = []string{"fcm.googleapis.com", "updates.push.services.mozilla.com", "web.push.apple.com"}

const windowsSuffix = ".notify.windows.com"

// Keys are the gateway's VAPID key pair, which identifies it to push services.
type Keys struct{ private *ecdsa.PrivateKey }

// LoadKeys reads the key pair kept in dir, creating it the first time.
func LoadKeys(dir string) (*Keys, error) {
	path := filepath.Join(dir, "vapid.json")
	var saved struct {
		Private string `json:"private"`
	}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &saved); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(saved.Private)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		k, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return &Keys{k}, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	raw, err := k.Bytes()
	if err != nil {
		return nil, err
	}
	saved.Private = base64.RawURLEncoding.EncodeToString(raw)
	b, _ = json.Marshal(saved)
	if err := writeNew(path, b); err != nil {
		if errors.Is(err, os.ErrExist) { // another gateway made it first
			return LoadKeys(dir)
		}
		return nil, err
	}
	return &Keys{k}, nil
}

// Public is the public key browsers subscribe with, as applicationServerKey
// takes it: an uncompressed point, base64url.
func (k *Keys) Public() string {
	b, _ := k.private.PublicKey.Bytes()
	return base64.RawURLEncoding.EncodeToString(b)
}

// Subscription is where one browser takes messages, and its keys.
type Subscription struct {
	Endpoint string
	P256DH   string // base64url
	Auth     string // base64url
}

// Message is one push: its payload and how the push service should treat it.
type Message struct {
	Payload []byte
	// TTL is how long the push service keeps an undelivered message.
	TTL time.Duration
	// Urgency is very-low, low, normal or high.
	Urgency string
	// Topic names what the message is about: an undelivered message is
	// replaced by a newer one on the same topic. At most 32 characters of
	// the base64url alphabet.
	Topic string
}

// Client sends messages.
type Client struct {
	Keys *Keys
	// Subject is how a push service may contact the sender: an https URL or
	// a mailto address.
	Subject string
	// Extra are additional push services: a host, or host:port, exactly.
	Extra []string
	// HTTP sends the request. Nil means a client that follows the
	// environment's proxy, never follows a redirect and gives up after
	// fifteen seconds.
	HTTP *http.Client
	now  func() time.Time
}

var defaultHTTP = &http.Client{
	Timeout:       15 * time.Second,
	Transport:     &http.Transport{Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: 10 * time.Second},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Allowed reports whether endpoint is an https URL of a known push service.
func (c *Client) Allowed(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" {
		return errors.New("a push endpoint is an https URL")
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	standard := port == "" || port == "443"
	// An extra service is a host, or a host and port, exactly.
	for _, s := range c.Extra {
		if strings.EqualFold(u.Host, s) || (standard && strings.EqualFold(host, s)) {
			return nil
		}
	}
	if !standard {
		return fmt.Errorf("push endpoint %s uses port %s", host, port)
	}
	for _, s := range Services {
		if host == s {
			return nil
		}
	}
	if strings.HasSuffix(host, windowsSuffix) && len(host) > len(windowsSuffix) {
		return nil
	}
	return fmt.Errorf("%s is not a known push service; add it with --push-service", host)
}

// Send encrypts m for sub and hands it to sub's push service.
func (c *Client) Send(ctx context.Context, sub Subscription, m Message) error {
	if err := c.Allowed(sub.Endpoint); err != nil {
		return err
	}
	body, err := Encrypt(sub, m.Payload)
	if err != nil {
		return err
	}
	auth, err := c.vapid(sub.Endpoint)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(int(m.TTL/time.Second)))
	if m.Urgency != "" {
		req.Header.Set("Urgency", m.Urgency)
	}
	if m.Topic != "" {
		req.Header.Set("Topic", m.Topic)
	}
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return ErrGone
	}
	if why := reason(detail); why != "" {
		return fmt.Errorf("push service answered %s: %s", resp.Status, why)
	}
	return fmt.Errorf("push service answered %s", resp.Status)
}

// reason is what a push service said about a refusal, on one short line:
// Apple's "reason", or the "message" or "error" of others, or the start of
// the body.
func reason(body []byte) string {
	var doc map[string]any
	text := string(body)
	if json.Unmarshal(body, &doc) == nil {
		for _, k := range []string{"reason", "message", "error"} {
			if s, ok := doc[k].(string); ok && s != "" {
				text = s
				break
			}
		}
	}
	text = strings.Join(strings.FieldsFunc(text, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if len(text) > 200 {
		text = text[:200]
	}
	return text
}

// vapid is the Authorization header that identifies the gateway to the push
// service of endpoint: an ES256 token for the service's origin, valid for
// twelve hours, and the key that verifies it.
func (c *Client) vapid(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	claims, _ := json.Marshal(map[string]any{
		"aud": u.Scheme + "://" + u.Host,
		"exp": now().Add(12 * time.Hour).Unix(),
		"sub": c.Subject,
	})
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`)) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, c.Keys.private, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64) // JWS wants r and s side by side, not ASN.1
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "vapid t=" + signing + "." + enc.EncodeToString(sig) + ", k=" + c.Keys.Public(), nil
}

// recordSize is the one record a message fits in.
const recordSize = 4096

// Encrypt returns the aes128gcm body that carries payload to sub alone.
func Encrypt(sub Subscription, payload []byte) ([]byte, error) {
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return encrypt(sub, payload, as, salt)
}

// encrypt is Encrypt with the sender's one-time key pair and salt given.
func encrypt(sub Subscription, payload []byte, as *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	uaPublic, err := decode64(sub.P256DH)
	if err != nil {
		return nil, fmt.Errorf("subscription key: %w", err)
	}
	secret, err := decode64(sub.Auth)
	if err != nil || len(secret) != 16 {
		return nil, errors.New("subscription auth secret is not 16 bytes")
	}
	ua, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("subscription key: %w", err)
	}
	// An 86-byte header and the 16-byte tag leave the rest of one record,
	// less the delimiter, for the payload.
	if len(payload) > recordSize-86-16-1 {
		return nil, errors.New("push payload is too large")
	}
	shared, err := as.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asPublic := as.PublicKey().Bytes()
	// RFC 8291 section 3.4: the shared secret and the auth secret make the
	// input keying material; the salt makes the content key and nonce.
	prkKey := hmacSum(secret, shared)
	keyInfo := append(append([]byte("WebPush: info\x00"), uaPublic...), asPublic...)
	ikm := hmacSum(prkKey, append(keyInfo, 1))
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 0, 86)
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint32(header, recordSize)
	header = append(header, byte(len(asPublic)))
	header = append(header, asPublic...)
	plain := append(append([]byte(nil), payload...), 2) // the last record's delimiter
	return gcm.Seal(header, nonce, plain, nil), nil
}

func hmacSum(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// decode64 reads base64url as browsers write it, padded or not.
func decode64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// writeNew creates path with b, readable by its owner alone, failing with
// os.ErrExist when it is there already.
func writeNew(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// A link never replaces what is there, so two gateways keep one pair.
	return os.Link(tmp.Name(), path)
}
