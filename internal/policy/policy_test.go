package policy

import "testing"

func TestConservativeRealmPolicy(t *testing.T) {
	for _, tc := range []struct {
		spec   Spec
		values []int
		want   int
	}{
		{Watch, nil, 32}, {Watch, []int{32, 2, 5}, 2}, {Watch, []int{32, 0}, 0},
		{Reconcile, []int{2, 8}, 8}, {Reconcile, []int{0, 8}, 0},
		{Observation, []int{900, 30}, 900}, {Diagnostics, []int{1, 0}, 0},
	} {
		if got := combine(tc.spec, tc.values); got != tc.want {
			t.Fatalf("%s %v: %d", tc.spec.Name, tc.values, got)
		}
	}
	for _, value := range []string{"-1", "0", "257", "unlimited"} {
		if _, err := Parse(Watch, value); err == nil {
			t.Fatalf("admitted %s", value)
		}
	}
}
