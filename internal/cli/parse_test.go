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

func TestBinaryCheck(t *testing.T) {
	self := filepath.Join(t.TempDir(), "tmag")
	other := filepath.Join(t.TempDir(), "tmag")
	for _, f := range []string{self, other} {
		if err := os.WriteFile(f, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name   string
		found  []string
		status string
	}{
		{"missing", nil, "fail"},
		{"same binary", []string{self}, "ok"},
		{"other binary first", []string{other, self}, "warn"},
	} {
		if got := binaryCheck(self, c.found); got.Status != c.status {
			t.Errorf("%s: status %s (%s), want %s", c.name, got.Status, got.Detail, c.status)
		}
	}
}
