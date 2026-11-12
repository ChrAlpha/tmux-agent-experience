package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicHTTPContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-only-credential" {
			t.Error("credential header missing")
		}
		w.Header().Set("Tmag-API-Schema", "tmag/http/v2")
		switch r.URL.Path {
		case "/v2/watch":
			if r.URL.Query().Get("since") != "11" {
				t.Error("cursor lost")
			}
			fmt.Fprint(w, "event: ping\ndata: 1\n\nid: 12\nevent: doc\ndata: {\"schema\":\"tmag/v2\",\"seq\":12}\n\n")
		case "/v2/act":
			var a map[string]any
			_ = json.NewDecoder(r.Body).Decode(&a)
			if a["target"] != "--literal" || a["text"] != "hello" || a["verb"] != "send" {
				t.Error("action changed")
			}
			fmt.Fprint(w, `{"schema":"tmag/v2"}`)
		case "/v2/request":
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"schema":"tmag/v2","error":{"code":"refused:changed"}}`)
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
		}
	}))
	defer server.Close()
	for _, v := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"watch", "--since", "11"}, 0, "\"seq\":12"},
		{[]string{"send", "--json", "--", "--literal", "-"}, 0, "tmag/v2"},
		{[]string{"request", "--json", "--", "session", "request"}, 3, "refused:changed"},
	} {
		var out, err bytes.Buffer
		code := Run(context.Background(), server.URL, "test-only-credential", v.args, strings.NewReader("hello"), &out, &err)
		if code != v.code || !strings.Contains(out.String(), v.want) {
			t.Fatalf("%v: %d %s", v.args, code, err.String())
		}
	}
}
func TestCredentialNeverFollowsRedirect(t *testing.T) {
	reached := false
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	code := Run(context.Background(), origin.URL, "test-only-credential", []string{"list"}, nil, io.Discard, io.Discard)
	if code == 0 || reached {
		t.Fatal("credential followed redirect")
	}
}
