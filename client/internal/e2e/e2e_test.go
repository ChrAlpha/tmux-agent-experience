// Package e2e drives the gateway against real tmag binaries and a fake agent
// in a private tmux server. It builds tmag and the fake agent from the
// repository this client belongs to, and skips when tmux is not installed.
package e2e

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/client/internal/auth"
	"github.com/ChrAlpha/tmux-agent-experience/client/internal/gateway"
	"github.com/ChrAlpha/tmux-agent-experience/client/internal/push"
)

var (
	binDir     string
	repoRoot   string
	clientRoot string
)

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("tmux"); err != nil {
		fmt.Println("tmux not installed; skipping client e2e tests")
		os.Exit(0)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0}); err != nil {
		panic(err)
	}
	if err := os.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0"); err != nil {
		panic(err)
	}
	// Never touch the developer's own tmux server.
	os.Unsetenv("TMUX")
	os.Unsetenv("TMUX_PANE")
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	repoRoot = filepath.Clean(filepath.Join(wd, "..", "..", ".."))
	clientRoot = filepath.Join(repoRoot, "client")
	dir, err := os.MkdirTemp("", "tmag-client-e2e-bin")
	if err != nil {
		panic(err)
	}
	binDir = dir
	build("./cmd/tmag", repoRoot, "tmag")
	build("./internal/e2e/fakeagent", repoRoot, "claude")
	build("./cmd/tmag-client", clientRoot, "tmag-client")
	os.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func build(pkg, dir, out string) {
	cmd := exec.Command("go", "build", "-o", filepath.Join(binDir, out), pkg)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		panic(fmt.Sprintf("build %s: %v", pkg, err))
	}
}

// world is one private tmux server and its state directories.
type world struct {
	t     *testing.T
	name  string
	sock  string
	dir   string
	state string
}

func startWorld(t *testing.T) *world {
	t.Helper()
	// Socket paths are limited to about 104 bytes; keep the tmpdir short.
	sock, err := os.MkdirTemp("/tmp", "tmc")
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, name: "e2e-client", sock: sock, dir: t.TempDir(), state: filepath.Join(sock, "state")}
	t.Setenv("TMUX_TMPDIR", sock)
	t.Setenv("TMAG_STATE_DIR", w.state)
	t.Setenv("TMAG_AUTH_DIR", filepath.Join(w.dir, "auth"))
	t.Setenv("CODEX_HOME", filepath.Join(w.dir, "codex-home"))
	server := exec.Command("tmux", "-L", w.name, "-f", "/dev/null", "new-session", "-d", "-s", "main", "-x", "200", "-y", "50", "/bin/sh")
	server.Env = append(os.Environ(), "SHELL=/bin/sh")
	if out, err := server.CombinedOutput(); err != nil {
		t.Fatalf("start tmux: %v %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", w.name, "kill-server").Run()
		os.RemoveAll(sock)
	})
	return w
}

func (w *world) tmux(args ...string) string {
	w.t.Helper()
	out, err := exec.Command("tmux", append([]string{"-L", w.name}, args...)...).CombinedOutput()
	if err != nil {
		w.t.Fatalf("tmux %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// spawn runs the fake agent in a new window and returns its pane id.
func (w *world) spawn(env map[string]string, argv ...string) string {
	w.t.Helper()
	if env == nil {
		env = map[string]string{}
	}
	args := []string{"new-window", "-d", "-P", "-F", "#{pane_id}", "-c", w.dir}
	env["TMAG_BIN"] = filepath.Join(binDir, "tmag")
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	argv = append([]string{"/bin/sh", "-c", `exec "$@"`, "sh"}, argv...)
	return w.tmux(append(args, argv...)...)
}

// client runs the tmag-client built from this module.
func (w *world) client(args ...string) []byte {
	w.t.Helper()
	cmd := exec.Command(filepath.Join(binDir, "tmag-client"), args...)
	cmd.Dir = clientRoot
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			stderr = string(exit.Stderr)
		}
		w.t.Fatalf("tmag-client %v: %v %s", args, err, stderr)
	}
	return out
}

// gatewayServer is the gateway under test and its HTTP page API. A real
// serve runs in a tmux window as a person does; a push test keeps the
// gateway in this process so it can reach a fake service.
type gatewayServer struct {
	t     *testing.T
	world *world
	gw    *gateway.Server
	hs    *httptest.Server
	base  string
	pane  string
	token string
	once  sync.Once
}

// serve starts tmag-client serve in a window of the private server, where its
// tmag commands are a person's, whoever runs go test.
func (w *world) serve(t *testing.T) *gatewayServer {
	t.Helper()
	port := freePort(t)
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	pane := w.spawn(nil, filepath.Join(binDir, "tmag-client"), "serve",
		"--machine", "local="+filepath.Join(binDir, "tmag"),
		"--state-dir", w.state,
		"--addr", "127.0.0.1:"+strconv.Itoa(port))
	g := &gatewayServer{t: t, world: w, base: base, pane: pane}
	t.Cleanup(g.close)
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		res, err := http.Get(base + "/api/machines")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusUnauthorized {
				return g
			}
		}
		if time.Now().After(deadline) {
			out, _ := exec.Command("tmux", "-L", w.name, "capture-pane", "-p", "-t", pane).CombinedOutput()
			t.Fatalf("serve did not listen on %s: %s", base, out)
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// gateway runs a gateway in this process, for push tests that need their own
// transport. Watching and alerts carry no caller.
func (w *world) gateway(t *testing.T, pushClient *push.Client) *gatewayServer {
	t.Helper()
	cfg := gateway.Config{
		Machines: []gateway.Machine{{Name: "local", Argv: []string{filepath.Join(binDir, "tmag")}}},
		StateDir: w.state,
		Log:      nil,
	}
	if pushClient != nil {
		cfg.Push = []string{"blocked", "finished"}
		cfg.PushClient = pushClient
		cfg.Owner = "me"
		cfg.Tailscale = []string{"box.example.ts.net"}
	}
	gw, err := gateway.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(gw)
	g := &gatewayServer{t: t, world: w, gw: gw, hs: hs, base: hs.URL}
	t.Cleanup(g.close)
	return g
}

func (g *gatewayServer) close() {
	g.once.Do(func() {
		if g.gw != nil {
			g.gw.Close()
			g.hs.Close()
		}
		if g.pane != "" {
			_ = exec.Command("tmux", "-L", g.world.name, "kill-window", "-t", g.pane).Run()
		}
	})
}

// do calls the page API, with this device's credential when it has one.
func (g *gatewayServer) do(method, path string, body any) (*http.Response, []byte) {
	g.t.Helper()
	var in io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		in = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, g.base+path, in)
	if err != nil {
		g.t.Fatal(err)
	}
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res, b
}

func (g *gatewayServer) json(method, path string, body, out any) *http.Response {
	g.t.Helper()
	res, b := g.do(method, path, body)
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			g.t.Fatalf("%s %s: %v %q", method, path, err, b)
		}
	}
	return res
}

// pair runs auth pair and redeems its code through the page API.
func (g *gatewayServer) pair() (auth.Generated, string) {
	g.t.Helper()
	out := g.world.client("auth", "pair", "phone", "--state-dir", g.world.state, "--json")
	var invitation struct {
		Pairing struct {
			Code string `json:"code"`
		} `json:"pairing"`
	}
	if err := json.Unmarshal(out, &invitation); err != nil || invitation.Pairing.Code == "" {
		g.t.Fatalf("auth pair: %v %q", err, out)
	}
	var paired struct {
		Credential auth.Generated `json:"credential"`
	}
	res := g.json(http.MethodPost, "/api/pair", map[string]string{"code": invitation.Pairing.Code}, &paired)
	if res.StatusCode != http.StatusOK || paired.Credential.Token == "" {
		g.t.Fatalf("pair: %s %+v", res.Status, paired)
	}
	g.token = paired.Credential.Token
	return paired.Credential, invitation.Pairing.Code
}

type snapshot struct {
	Type     string            `json:"type"`
	Seq      int64             `json:"seq"`
	Sessions []gateway.Session `json:"sessions"`
	Panes    []gateway.Pane    `json:"panes"`
}

// snapshot takes the gateway's current view from its first snapshot frame.
func (g *gatewayServer) snapshot() snapshot {
	g.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+"/api/watch", nil)
	if err != nil {
		g.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		g.t.Fatalf("watch: %s", res.Status)
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var frame struct {
			Machine string   `json:"machine"`
			Doc     snapshot `json:"doc"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame) != nil {
			continue
		}
		if frame.Machine == "local" && frame.Doc.Type == "snapshot" {
			return frame.Doc
		}
	}
	g.t.Fatalf("watch ended before a snapshot: %v", sc.Err())
	return snapshot{}
}

func (g *gatewayServer) waitSession(id string, pred func(gateway.Session) bool) gateway.Session {
	g.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range g.snapshot().Sessions {
			if s.ID == id && pred(s) {
				return s
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	g.t.Logf("sessions at timeout: %+v", g.snapshot().Sessions)
	g.t.Fatalf("session %s did not reach the expected state", id)
	return gateway.Session{}
}

func (g *gatewayServer) waitIdle(id string) gateway.Session {
	g.t.Helper()
	return g.waitSession(id, func(s gateway.Session) bool { return s.State == "idle" && s.Turn != "" })
}

func (g *gatewayServer) send(id, text string) {
	g.t.Helper()
	res, b := g.do(http.MethodPost, "/api/act", map[string]string{"machine": "local", "verb": "send", "target": id, "text": text})
	if res.StatusCode != http.StatusOK {
		g.t.Fatalf("send: %s %s", res.Status, b)
	}
}

func (g *gatewayServer) readLast(id string) string {
	g.t.Helper()
	var doc struct {
		Text string `json:"text"`
	}
	res := g.json(http.MethodGet, "/api/read?machine=local&target="+url.QueryEscape(id)+"&last=1", nil, &doc)
	if res.StatusCode != http.StatusOK {
		g.t.Fatalf("read: %s", res.Status)
	}
	return doc.Text
}

// pushService is a fake Web Push service that decrypts what it receives.
type pushService struct {
	srv  *httptest.Server
	ua   *ecdh.PrivateKey
	auth []byte
	got  chan pushRecord
}

type pushRecord struct {
	msg     push.Message
	fields  map[string]any
	headers http.Header
}

func newPushService(t *testing.T) *pushService {
	t.Helper()
	ua, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	p := &pushService{ua: ua, auth: auth, got: make(chan pushRecord, 16)}
	p.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		plain, err := decrypt(body, p.ua, p.auth)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var msg push.Message
		var fields map[string]any
		if json.Unmarshal(plain, &msg) != nil || json.Unmarshal(plain, &fields) != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		p.got <- pushRecord{msg: msg, fields: fields, headers: r.Header.Clone()}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *pushService) client() *push.Client {
	return &push.Client{HTTP: p.srv.Client(), Endpoint: url.Parse}
}

func (p *pushService) subscribe(g *gatewayServer) {
	g.t.Helper()
	body := map[string]any{
		"endpoint": p.srv.URL + "/push",
		"keys": map[string]string{
			"p256dh": base64.RawURLEncoding.EncodeToString(p.ua.PublicKey().Bytes()),
			"auth":   base64.RawURLEncoding.EncodeToString(p.auth),
		},
	}
	res, b := g.do(http.MethodPost, "/api/push", body)
	if res.StatusCode != http.StatusOK {
		g.t.Fatalf("subscribe: %s %s", res.Status, b)
	}
}

// decrypt reads one aes128gcm record, as RFC 8188 and RFC 8291 define it.
func decrypt(body []byte, ua *ecdh.PrivateKey, authSecret []byte) ([]byte, error) {
	if len(body) < 16+4+1+65 {
		return nil, fmt.Errorf("short push body")
	}
	salt := body[:16]
	idlen := int(body[20])
	if idlen > len(body)-21 {
		return nil, fmt.Errorf("short push key id")
	}
	asBytes := body[21 : 21+idlen]
	ciphertext := body[21+idlen:]
	as, err := ecdh.P256().NewPublicKey(asBytes)
	if err != nil {
		return nil, err
	}
	shared, err := ua.ECDH(as)
	if err != nil {
		return nil, err
	}
	info := append([]byte("WebPush: info\x00"), ua.PublicKey().Bytes()...)
	info = append(info, asBytes...)
	prk, err := hkdf.Extract(sha256.New, shared, authSecret)
	if err != nil {
		return nil, err
	}
	ikm, err := hkdf.Expand(sha256.New, prk, string(info), 32)
	if err != nil {
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
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}
	if len(plain) == 0 || plain[len(plain)-1] != 0x02 {
		return nil, fmt.Errorf("missing last-record delimiter")
	}
	return plain[:len(plain)-1], nil
}
