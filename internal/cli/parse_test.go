package cli

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseInterleaved(t *testing.T) {
	cases := []struct {
		args    []string
		pos     []string
		wait    bool
		timeout string
	}{
		{[]string{"%3", "hello", "world"}, []string{"%3", "hello", "world"}, false, ""},
		{[]string{"%3", "--wait", "hello"}, []string{"%3", "hello"}, true, ""},
		{[]string{"--wait", "%3", "--timeout", "5s", "hi"}, []string{"%3", "hi"}, true, "5s"},
		{[]string{"%3", "--", "--wait", "-x"}, []string{"%3", "--wait", "-x"}, false, ""},
	}
	for _, c := range cases {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		wait := fs.Bool("wait", false, "")
		timeout := fs.String("timeout", "", "")
		pos, err := parse(fs, c.args)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if !reflect.DeepEqual(pos, c.pos) || *wait != c.wait || *timeout != c.timeout {
			t.Fatalf("%v: pos=%v wait=%v timeout=%q", c.args, pos, *wait, *timeout)
		}
	}
	if _, err := parse(flag.NewFlagSet("t", flag.ContinueOnError), []string{"--nope"}); err == nil {
		t.Fatal("unknown flag accepted")
	}
}

func TestRemoveBinary(t *testing.T) {
	data := t.TempDir()
	installed := filepath.Join(data, "installs", "github-x", "0.1.0", "tmag")
	for _, dir := range []string{filepath.Dir(installed), filepath.Join(data, "shims")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := removeBinary(installed); got != "mise unuse -g "+miseTool {
		t.Errorf("mise install: %q", got)
	}
	if got := removeBinary("/home/me/.local/bin/tmag"); got != "rm /home/me/.local/bin/tmag" {
		t.Errorf("plain install: %q", got)
	}
}

func TestBinaryCheck(t *testing.T) {
	// A mise data directory with a shim (a link to the mise binary) and a
	// versioned install of tmag.
	data := t.TempDir()
	mise := filepath.Join(t.TempDir(), "mise")
	installed := filepath.Join(data, "installs", "github-x", "0.1.0", "tmag")
	shim := filepath.Join(data, "shims", "tmag")
	other := filepath.Join(t.TempDir(), "tmag")
	for _, f := range []string{mise, installed, other} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(mise, shim); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name   string
		self   string
		found  []string
		status string
	}{
		{"missing", installed, nil, "fail"},
		{"shim first", installed, []string{shim}, "ok"},
		{"run through a shim", installed, []string{installed, shim}, "ok"},
		{"versioned dir only", installed, []string{installed}, "warn"},
		{"same binary", other, []string{other}, "ok"},
		{"other binary", other, []string{installed + "-nope", other}, "warn"},
	} {
		if got := binaryCheck(c.self, c.found); got.Status != c.status {
			t.Errorf("%s: status %s (%s), want %s", c.name, got.Status, got.Detail, c.status)
		}
	}
}
