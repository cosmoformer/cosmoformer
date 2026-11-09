package awsaccess

import (
	"strings"
	"testing"
)

func TestExactRoleAndSessionMatching(t *testing.T) {
	r, ok := parseRole(bootstrapARN)
	if !ok {
		t.Fatal("role path rejected")
	}
	for _, c := range []struct {
		arn, account, session string
		want                  bool
	}{
		{bootstrapARN, "111122223333", "", true},
		{"arn:aws:iam::111122223333:role/bootstrap", "111122223333", "", false},
		{"arn:aws:sts::111122223333:assumed-role/bootstrap/i-synthetic", "111122223333", "", true},
		{"arn:aws:sts::111122223333:assumed-role/bootstrap/selected-session", "111122223333", "selected-session", true},
		{"arn:aws:sts::111122223333:assumed-role/bootstrap/other-session", "111122223333", "selected-session", false},
		{"arn:aws:sts::111122223333:assumed-role/bootstrap-suffix/i-synthetic", "111122223333", "", false},
		{"arn:aws:sts::111122223333:assumed-role/workers/bootstrap/i-synthetic", "111122223333", "", false},
		{"arn:aws:sts::111122223333:assumed-role/bootstrap/", "111122223333", "", false},
		{"arn:aws:sts::111122223333:assumed-role/bootstrap/session/extra", "111122223333", "", false},
		{"arn:aws:sts::111122223333:assumed-role/bootstrap/session", "999900001111", "", false},
		{"arn:aws-cn:sts::111122223333:assumed-role/bootstrap/session", "111122223333", "", false},
		{"arn:aws:sts:eu-west-1:111122223333:assumed-role/bootstrap/session", "111122223333", "", false},
		{"arn:aws:iam::111122223333:user/bootstrap", "111122223333", "", false},
	} {
		if r.matches(c.arn, c.account, c.session) != c.want {
			t.Error("unexpected role match", c.arn)
		}
	}
	for _, invalid := range []string{"", "not-an-arn", "arn:aws:iam::111122223333:role/", "arn:aws:iam::111122223333:role//bootstrap", "arn:aws:iam::111122223333:role/a//bootstrap", "arn:aws:iam::111122223333:role/bootstrap/", "arn:aws:iam::111122223333:role/boot*", "arn:aws:iam::111122223333:role/space path/bootstrap", "arn:aws:iam::111122223333:role/" + strings.Repeat("r", 65), "arn:aws-iso:iam::111122223333:role/bootstrap", "arn:aws:iam:eu-west-1:111122223333:role/bootstrap", "arn:aws:iam::1234:role/bootstrap", "arn:aws:sts::111122223333:assumed-role/bootstrap/session"} {
		if _, ok := parseRole(invalid); ok {
			t.Error("invalid expected role accepted", invalid)
		}
	}
	valid := "arn:aws:sts::111122223333:assumed-role/bootstrap/session"
	if !r.identity(valid, "111122223333", "AROABBBBBBBBBBBBBBBBB:session", "") {
		t.Fatal("valid identity rejected")
	}
	for _, id := range []string{"", "malformed", "AIDABBBBBBBBBBBBBBBBB:session", "AROABBBBBBBBBBBBBBBBB:other", "AROABBBBBBBBBBBBBBBBB:session:extra"} {
		if r.identity(valid, "111122223333", id, "") {
			t.Fatal("ambiguous user identity accepted")
		}
	}
	if !r.identity(bootstrapARN, "111122223333", "AROABBBBBBBBBBBBBBBBB", "") {
		t.Fatal("exact IAM role identity rejected")
	}
}
