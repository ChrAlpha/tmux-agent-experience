package machine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HTTP reaches tmag through an endpoint of `tmag serve`, with a credential
// `tmag auth` issued there.
type HTTP struct {
	base   string
	token  string
	client *http.Client
	// silence is how long a watch may go without a frame; serve pings every
	// fifteen seconds.
	silence time.Duration
}

// NewHTTP returns the backend for endpoint: https, or http on loopback.
func NewHTTP(endpoint, token string) (*HTTP, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("endpoint %q is not a URL such as https://box.example.net", endpoint)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if ip := net.ParseIP(u.Hostname()); u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, fmt.Errorf("endpoint %s: plain http only on loopback; use https", endpoint)
		}
	default:
		return nil, fmt.Errorf("endpoint %s: use https", endpoint)
	}
	if token = strings.TrimSpace(token); token == "" {
		return nil, fmt.Errorf("endpoint %s has no credential", endpoint)
	}
	return &HTTP{
		base:  strings.TrimRight(u.String(), "/"),
		token: token,
		client: &http.Client{
			Transport:     &http.Transport{Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 2*time.Minute + 10*time.Second},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		silence: 45 * time.Second,
	}, nil
}

// ReadToken reads a credential file: the token, or the document
// `tmag auth issue --json` prints.
func ReadToken(b []byte) (string, error) {
	text := strings.TrimSpace(string(b))
	if !strings.HasPrefix(text, "{") {
		return text, nil
	}
	var issued struct {
		Credential struct {
			Token string `json:"token"`
		} `json:"credential"`
	}
	if err := json.Unmarshal(b, &issued); err != nil || issued.Credential.Token == "" {
		return "", errors.New("the credential file holds neither a token nor tmag auth's document")
	}
	return issued.Credential.Token, nil
}

func (h *HTTP) String() string { return h.base }

func (h *HTTP) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Code: Unreachable, Message: err.Error()}
	}
	return resp, nil
}

// Call runs one verb through the endpoint.
func (h *HTTP) Call(ctx context.Context, op Op) (json.RawMessage, error) {
	var resp *http.Response
	var err error
	q := url.Values{"target": {op.Target}}
	switch op.Verb {
	case "read":
		if op.Last {
			q.Set("last", "1")
		} else {
			q.Set("lines", strconv.Itoa(op.Lines))
		}
		resp, err = h.do(ctx, http.MethodGet, "/v2/read?"+q.Encode(), nil)
	case "request":
		q.Set("request", op.Request)
		resp, err = h.do(ctx, http.MethodGet, "/v2/request?"+q.Encode(), nil)
	case "send", "interrupt", "answer":
		body, _ := json.Marshal(map[string]any{"verb": op.Verb, "target": op.Target, "text": op.Text, "request": op.Request,
			"decision": op.Decision, "message": op.Message, "answers": op.Answers})
		resp, err = h.do(ctx, http.MethodPost, "/v2/act", bytes.NewReader(compact(body)))
	default:
		return nil, fmt.Errorf("unknown verb %q", op.Verb)
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDoc))
	if err != nil {
		return nil, &Error{Code: Unreachable, Message: err.Error()}
	}
	if resp.StatusCode == http.StatusOK && json.Valid(b) {
		return b, nil
	}
	if e := failure(b, ""); e.Code != Unreachable {
		return nil, e
	}
	return nil, &Error{Code: Unreachable, Message: "tmag serve answered " + resp.Status}
}

// compact drops the empty fields of an action, which serve refuses to guess.
func compact(b []byte) []byte {
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for k, v := range m {
		if v == nil || v == "" {
			delete(m, k)
		}
	}
	out, _ := json.Marshal(m)
	return out
}

// Watch follows the endpoint's event stream.
func (h *HTTP) Watch(ctx context.Context, since int64, each func(Doc) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	path := "/v2/watch"
	if since >= 0 {
		path += "?since=" + strconv.FormatInt(since, 10)
	}
	resp, err := h.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if e := failure(b, ""); e.Code != Unreachable {
			return e
		}
		return &Error{Code: Unreachable, Message: "tmag serve answered " + resp.Status}
	}
	errSilent := &Error{Code: Unreachable, Message: "tmag serve went silent"}
	quiet := time.AfterFunc(h.silence, func() { cancel(errSilent) })
	defer quiet.Stop()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), maxDoc)
	event, data := "", ""
	for sc.Scan() {
		quiet.Reset(h.silence)
		line := sc.Text()
		switch {
		case line == "":
			if event == "doc" {
				var d Doc
				if json.Unmarshal([]byte(data), &d) == nil && d.Type != "" {
					if err := each(d); err != nil {
						return err
					}
				}
			}
			event, data = "", ""
		case strings.HasPrefix(line, ":"):
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				event = value
			case "data":
				if data != "" {
					data += "\n"
				}
				data += value
			}
		}
	}
	if cause := context.Cause(ctx); cause != nil && ctx.Err() != nil {
		return cause
	}
	if sc.Err() != nil {
		return &Error{Code: Unreachable, Message: sc.Err().Error()}
	}
	return &Error{Code: Unreachable, Message: "tmag serve ended the stream"}
}
