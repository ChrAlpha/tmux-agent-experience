// Package httpclient adapts public tmag CLI operations to tmag/http/v2.
// It lets the gateway use an HTTP endpoint alongside SSH and container commands.
package httpclient

import (
	"bufio"
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

	"github.com/ChrAlpha/tmux-agent-experience/client/internal/version"
)

func Endpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("endpoint must be an HTTP(S) origin without userinfo, query or fragment")
	}
	if u.Scheme == "http" && u.Hostname() != "localhost" && !net.ParseIP(u.Hostname()).IsLoopback() {
		return nil, errors.New("remote HTTP endpoints require HTTPS")
	}
	if strings.Trim(u.Path, "/") != "" {
		return nil, errors.New("endpoint must be an origin without a path")
	}
	return u, nil
}

// Run writes a normal tmag/v2 document or watch stream. The gateway continues
// to use the same public command contract for every transport.
func Run(ctx context.Context, endpoint, token string, args []string, in io.Reader, out, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "an operation is required")
		return 2
	}
	u, err := Endpoint(endpoint)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	verb := args[0]
	flags := map[string]string{}
	var pos []string
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "--") {
			pos = append(pos, arg)
			continue
		}
		name, value, has := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if name == "json" || name == "last" {
			flags[name] = "1"
			continue
		}
		if !has {
			i++
			if i >= len(args) {
				fmt.Fprintln(stderr, "missing flag value")
				return 2
			}
			value = args[i]
		}
		flags[name] = value
	}
	method := "GET"
	path := "/v2/" + verb
	q := url.Values{}
	var body io.Reader
	target := func() bool {
		if len(pos) == 0 {
			fmt.Fprintln(stderr, "target is required")
			return false
		}
		q.Set("target", pos[0])
		return true
	}
	switch verb {
	case "list":
	case "watch":
		if f := flags["since"]; f != "" {
			q.Set("since", f)
		}
	case "inspect":
		if !target() {
			return 2
		}
	case "request":
		if !target() || len(pos) != 2 {
			return 2
		}
		q.Set("request", pos[1])
	case "read":
		if !target() {
			return 2
		}
		if flags["last"] != "" {
			q.Set("last", "1")
		} else {
			n := flags["lines"]
			if n == "" {
				n = "60"
			}
			q.Set("lines", n)
		}
	case "send", "interrupt", "answer":
		if len(pos) == 0 {
			return 2
		}
		method = "POST"
		path = "/v2/act"
		a := map[string]any{"verb": verb, "target": pos[0]}
		if verb == "send" {
			b, err := io.ReadAll(io.LimitReader(in, 1<<20+1))
			if err != nil || len(b) > 1<<20 {
				fmt.Fprintln(stderr, "prompt exceeds request limit")
				return 2
			}
			a["text"] = string(b)
		}
		if verb == "answer" {
			if len(pos) != 3 {
				return 2
			}
			a["request"], a["decision"] = pos[1], pos[2]
			if m := flags["message"]; m != "" {
				a["message"] = m
			}
			if m := flags["answers"]; m != "" {
				var answers map[string]string
				if json.Unmarshal([]byte(m), &answers) != nil {
					return 2
				}
				a["answers"] = answers
			}
		}
		b, _ := json.Marshal(a)
		body = strings.NewReader(string(b))
	default:
		fmt.Fprintln(stderr, "unsupported HTTP operation")
		return 2
	}
	u.Path = path
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Credentials belong to this endpoint. Never follow a redirect to another
	// origin or accidentally rewrite an admitted POST into a GET.
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer res.Body.Close()
	if res.Header.Get("Tmag-API-Schema") != version.HTTP {
		fmt.Fprintln(stderr, "unsupported HTTP contract")
		return 1
	}
	if verb == "watch" && res.StatusCode == http.StatusOK {
		return stream(res.Body, out, stderr)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !json.Valid(b) {
		fmt.Fprintln(stderr, "endpoint returned no JSON document (HTTP "+strconv.Itoa(res.StatusCode)+")")
		return 1
	}
	_, _ = out.Write(b)
	if res.StatusCode == http.StatusOK {
		return 0
	}
	var d struct{ Error struct{ Code string } }
	_ = json.Unmarshal(b, &d)
	switch {
	case strings.HasPrefix(d.Error.Code, "refused:"):
		return 3
	case d.Error.Code == "timeout":
		return 4
	case d.Error.Code == "usage":
		return 2
	}
	return 1
}
func stream(in io.Reader, out, stderr io.Writer) int {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	event := ""
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if event == "doc" {
				b := strings.Join(data, "\n")
				if !json.Valid([]byte(b)) {
					fmt.Fprintln(stderr, "invalid watch document")
					return 1
				}
				if _, err := fmt.Fprintln(out, b); err != nil {
					return 1
				}
			}
			event = ""
			data = nil
			continue
		}
		if v, ok := strings.CutPrefix(line, "event:"); ok {
			event = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(v, " "))
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
