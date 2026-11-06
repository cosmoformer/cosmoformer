package access

import "testing"

func TestSeparateAuthorityAndInheritance(t *testing.T) {
	g := []Grant{{ProjectID: "p", Role: "administrator"}, {ProjectID: "p", EnvironmentID: "dev", Role: "deployer"}, {ProjectID: "p", Role: "approver"}}
	for _, tc := range []struct {
		p, e string
		a    Action
		want bool
	}{
		{"p", "prod", Manage, true}, {"p", "dev", Apply, true}, {"p", "prod", Apply, false},
		{"p", "", Apply, false}, {"p", "prod", Approve, true}, {"p", "prod", RawEvidenceRead, false},
		{"other", "dev", Manage, false}, {"", "", View, false},
	} {
		if got := Allows(g, tc.p, tc.e, tc.a); got != tc.want {
			t.Errorf("%+v: got %v", tc, got)
		}
	}
	if Allows([]Grant{{ProjectID: "p", Role: "administrator"}}, "p", "dev", View) {
		t.Fatal("administrator must not read infrastructure")
	}
}
