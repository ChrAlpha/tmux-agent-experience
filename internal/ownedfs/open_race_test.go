package ownedfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func rootDir(t *testing.T) (string, *Dir) {
	t.Helper()
	root := t.TempDir()
	d, err := Open(context.Background(), root, ".", true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return root, d
}

func TestExistingOpenCannotCreateThroughRacedSymlink(t *testing.T) {
	root, d := rootDir(t)
	slot, target := filepath.Join(root, "slot"), filepath.Join(root, "unrelated")
	if err := os.WriteFile(slot, []byte("old temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	opener := func(name string, flags int, perm os.FileMode) (*os.File, error) {
		if !called {
			called = true
			// This is after ownedfs's Lstat and before the actual Go1.25
			// Root.OpenFile, which follows relative symlinks despite O_NOFOLLOW.
			if err := os.Remove(slot); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("unrelated", slot); err != nil {
				t.Fatal(err)
			}
		}
		return d.current().OpenFile(name, flags, perm)
	}
	f, err := d.openUsing(context.Background(), "slot", os.O_CREATE|os.O_WRONLY, 0o600, opener)
	if f != nil {
		_ = f.Close()
	}
	if !called || !errors.Is(err, ErrScope) {
		t.Fatalf("raced open called=%v err=%v", called, err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created symlink target before rejecting alias: %v", err)
	}
	if got, err := os.Readlink(slot); err != nil || got != "unrelated" {
		t.Fatalf("changed replacement: %q %v", got, err)
	}
}

func TestCreateRetriesDisappearanceAndCompetingCreator(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "competing creator", true: "disappearing file"}[existing], func(t *testing.T) {
			root, d := rootDir(t)
			path := filepath.Join(root, "slot")
			if existing {
				if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var calls []int
			opener := func(name string, flags int, perm os.FileMode) (*os.File, error) {
				calls = append(calls, flags)
				if len(calls) == 1 {
					if existing {
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
					} else if err := os.WriteFile(path, []byte("created elsewhere"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return d.current().OpenFile(name, flags, perm)
			}
			f, err := d.openUsing(context.Background(), "slot", os.O_CREATE|os.O_RDWR, 0o600, opener)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if len(calls) != 2 {
				t.Fatalf("open attempts=%v", calls)
			}
			creating, opening := calls[0], calls[1]
			if existing {
				creating, opening = calls[1], calls[0]
			}
			if creating&(os.O_CREATE|os.O_EXCL) != (os.O_CREATE|os.O_EXCL) || opening&os.O_CREATE != 0 {
				t.Fatalf("unsafe open attempts=%v", calls)
			}
			if !existing {
				if got, err := os.ReadFile(path); err != nil || string(got) != "created elsewhere" {
					t.Fatalf("competing file changed: %q %v", got, err)
				}
			}
			if _, err := d.OpenFile(context.Background(), "slot", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); !errors.Is(err, os.ErrExist) {
				t.Fatalf("exclusive open reused existing file: %v", err)
			}
		})
	}
}

func TestOpenRetryHonorsCancellation(t *testing.T) {
	root, d := rootDir(t)
	path := filepath.Join(root, "slot")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	opener := func(name string, flags int, perm os.FileMode) (*os.File, error) {
		calls++
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		f, err := d.current().OpenFile(name, flags, perm)
		cancel()
		return f, err
	}
	f, err := d.openUsing(ctx, "slot", os.O_CREATE|os.O_RDWR, 0o600, opener)
	if f != nil {
		_ = f.Close()
	}
	if calls != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled retry attempts=%d err=%v", calls, err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created after cancellation: %v", err)
	}
}

func TestReadObservesRacedPublicationAgainWhileWriterRefuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove bool
		late   bool
	}{
		{"replaced before open", false, false},
		{"replaced after open", false, true},
		{"removed after open", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, d := rootDir(t)
			path, tmp := filepath.Join(root, "record"), filepath.Join(root, "tmp")
			publish := func(data string) {
				if err := os.WriteFile(tmp, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(tmp, path); err != nil {
					t.Fatal(err)
				}
			}
			race := func() {
				if !tc.remove {
					publish("new")
				} else if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			for _, read := range []bool{true, false} {
				publish("old")
				calls := 0
				opener := func(name string, flags int, perm os.FileMode) (*os.File, error) {
					calls++
					if calls == 1 && !tc.late {
						race()
					}
					f, err := d.current().OpenFile(name, flags, perm)
					if calls == 1 && tc.late {
						race()
					}
					return f, err
				}
				flags := os.O_RDONLY
				if !read {
					flags = os.O_RDWR
				}
				f, err := d.openUsing(context.Background(), "record", flags, 0, opener)
				var got []byte
				if err == nil {
					got, err = io.ReadAll(f)
					_ = f.Close()
				}
				switch {
				case !read:
					if calls != 1 || !errors.Is(err, ErrScope) {
						t.Fatalf("writer accepted a raced name: calls=%d err=%v", calls, err)
					}
				case tc.remove:
					if calls != 2 || !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("removal during read: calls=%d err=%v", calls, err)
					}
				default:
					if calls != 2 || err != nil || string(got) != "new" {
						t.Fatalf("raced read: calls=%d %q %v", calls, got, err)
					}
				}
			}
		})
	}
}
