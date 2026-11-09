package awsaccess

import (
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
)

var accountID = regexp.MustCompile(`^[0-9]{12}$`)
var roleName = regexp.MustCompile(`^[A-Za-z0-9_+=,.@-]{1,64}$`)
var sessionName = regexp.MustCompile(`^[A-Za-z0-9_+=,.@-]{2,64}$`)
var roleID = regexp.MustCompile(`^AROA[A-Z0-9]{17}$`)

type role struct{ partition, account, name, arn string }

func parseRole(value string) (role, bool) {
	a, e := arn.Parse(value)
	if e != nil || (a.Partition != "aws" && a.Partition != "aws-cn" && a.Partition != "aws-us-gov") || a.Service != "iam" || a.Region != "" || !accountID.MatchString(a.AccountID) || !strings.HasPrefix(a.Resource, "role/") {
		return role{}, false
	}
	path := strings.TrimPrefix(a.Resource, "role/")
	parts := strings.Split(path, "/")
	if len(path) > 577 || !roleName.MatchString(parts[len(parts)-1]) {
		return role{}, false
	}
	for _, p := range parts[:len(parts)-1] {
		if p == "" {
			return role{}, false
		}
		for _, c := range p {
			if c < '!' || c > '~' {
				return role{}, false
			}
		}
	}
	return role{a.Partition, a.AccountID, parts[len(parts)-1], value}, true
}

// IAM role names are unique within an account regardless of path. STS session
// ARNs omit the IAM path; compare the exact role name, partition and account,
// never a suffix/substring. A direct IAM ARN must match its full path exactly.
func (r role) matches(value, account, requiredSession string) bool {
	if account != r.account {
		return false
	}
	if requiredSession == "" && value == r.arn {
		return true
	}
	a, e := arn.Parse(value)
	if e != nil || a.Partition != r.partition || a.AccountID != r.account || a.Region != "" || a.Service != "sts" {
		return false
	}
	p := strings.Split(a.Resource, "/")
	return len(p) == 3 && p[0] == "assumed-role" && p[1] == r.name && sessionName.MatchString(p[2]) && (requiredSession == "" || p[2] == requiredSession)
}

func (r role) identity(value, account, userID, requiredSession string) bool {
	if !r.matches(value, account, requiredSession) {
		return false
	}
	if value == r.arn {
		return roleID.MatchString(userID)
	}
	a, _ := arn.Parse(value)
	session := strings.Split(a.Resource, "/")[2]
	id := strings.Split(userID, ":")
	return len(id) == 2 && roleID.MatchString(id[0]) && id[1] == session
}
