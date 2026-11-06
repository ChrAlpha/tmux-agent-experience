package serve

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Access is an application behind Cloudflare Access that a Cloudflare tunnel
// forwards to tmag serve: its host names, the team whose keys sign its
// tokens, and its audience tag.
type Access struct {
	Hosts    []string
	Team     string
	Audience string
}

// leeway allows for clocks that disagree a little.
const leeway = time.Minute

// accessKeys verifies Access tokens against the team's published keys. It
// fetches them when a token names a key it does not know, as after Access
// rotates its key, at most once a minute.
type accessKeys struct {
	issuer   string // https://<team>.cloudflareaccess.com
	certs    string // where the team publishes its keys
	audience string
	client   *http.Client
	now      func() time.Time

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

func newAccessKeys(a Access) *accessKeys {
	issuer := "https://" + a.Team + ".cloudflareaccess.com"
	return &accessKeys{
		issuer: issuer, certs: issuer + "/cdn-cgi/access/certs", audience: a.Audience,
		client: &http.Client{Timeout: 10 * time.Second}, now: time.Now, keys: map[string]*rsa.PublicKey{},
	}
}

// verify checks an Access token (Cf-Access-Jwt-Assertion) and returns the
// email it vouches for.
func (k *accessKeys) verify(ctx context.Context, token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("access: no token")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodePart(parts[0], &header); err != nil || header.Alg != "RS256" || header.Kid == "" {
		return "", errors.New("access: not an RS256 token")
	}
	var claims struct {
		Aud   audience `json:"aud"`
		Iss   string   `json:"iss"`
		Email string   `json:"email"`
		Exp   int64    `json:"exp"`
		Nbf   int64    `json:"nbf"`
	}
	if err := decodePart(parts[1], &claims); err != nil {
		return "", fmt.Errorf("access: claims: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", errors.New("access: signature not base64url")
	}
	key, err := k.key(ctx, header.Kid)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return "", errors.New("access: bad signature")
	}
	now := k.now()
	switch {
	case claims.Iss != k.issuer:
		return "", fmt.Errorf("access: issued by %q, not %q", claims.Iss, k.issuer)
	case !slices.Contains(claims.Aud, k.audience):
		return "", errors.New("access: for another application")
	case claims.Exp == 0 || now.After(time.Unix(claims.Exp, 0).Add(leeway)):
		return "", errors.New("access: expired")
	case claims.Nbf != 0 && now.Add(leeway).Before(time.Unix(claims.Nbf, 0)):
		return "", errors.New("access: not valid yet")
	case claims.Email == "":
		return "", errors.New("access: no email")
	}
	return claims.Email, nil
}

func decodePart(part string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// audience is the aud claim: one string or a list of them.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

func (k *accessKeys) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if key := k.keys[kid]; key != nil {
		return key, nil
	}
	if !k.fetched.IsZero() && k.now().Sub(k.fetched) < time.Minute {
		return nil, fmt.Errorf("access: unknown key %q", kid)
	}
	k.fetched = k.now()
	keys, err := k.fetch(ctx)
	if err != nil {
		return nil, err
	}
	k.keys = keys
	if key := keys[kid]; key != nil {
		return key, nil
	}
	return nil, fmt.Errorf("access: unknown key %q", kid)
}

func (k *accessKeys) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.certs, nil)
	if err != nil {
		return nil, err
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("access: keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("access: keys: %s", resp.Status)
	}
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("access: keys: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, j := range doc.Keys {
		n, nerr := base64.RawURLEncoding.DecodeString(j.N)
		e, eerr := base64.RawURLEncoding.DecodeString(j.E)
		if j.Kty != "RSA" || j.Kid == "" || nerr != nil || eerr != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		keys[j.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	return keys, nil
}
