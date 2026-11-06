package proc

import (
	"regexp"
	"testing"
)

func TestRealmNamesThisProcessTable(t *testing.T) {
	r := Realm()
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(r) {
		t.Fatalf("Realm() = %q, want 12 hex digits on a supported platform", r)
	}
	if again := Realm(); again != r {
		t.Errorf("Realm() changed within one process: %q, then %q", r, again)
	}
}

func TestRealmID(t *testing.T) {
	if got := realmID(""); got != "" {
		t.Errorf(`realmID("") = %q, want "" when the platform does not say`, got)
	}
	a, b := realmID("boot pid:[1]"), realmID("boot pid:[2]")
	if a == b {
		t.Errorf("two PID namespaces share realm %q", a)
	}
	if a != realmID("boot pid:[1]") {
		t.Error("realmID is not deterministic")
	}
}
