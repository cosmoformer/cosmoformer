package runs

import (
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Control carries the complete immutable process capability. Delivery is an
// observation, never a stop proof or replacement launch permission.
type Control struct {
	ID         string `json:"control_id"`
	Attempt    string `json:"attempt_id"`
	Worker     string `json:"worker_id"`
	Generation int64  `json:"generation"`
	Agent      string `json:"agent_instance"`
	Kind       string `json:"kind"`
	Ordinal    int64  `json:"command_ordinal"`
}
type StopProof struct {
	Method    string `json:"method" enum:"agent,host,operator"`
	Statement string `json:"statement" maxLength:"2048"`
	Reference string `json:"protected_reference" maxLength:"512"`
}
type Reconciliation struct {
	Complete   bool     `json:"complete"`
	Statement  string   `json:"statement" maxLength:"2048"`
	References []string `json:"protected_references" maxItems:"16"`
}
type RecoveryEvidence struct {
	Stop           StopProof      `json:"stop_proof"`
	Reconciliation Reconciliation `json:"reconciliation"`
}

func boundedStatement(s string) bool {
	if len(s) < 1 || len(s) > 2048 || !utf8.ValidString(s) || strings.TrimSpace(s) == "" {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) && c != '\n' && c != '\t' {
			return false
		}
	}
	return true
}
func protectedReference(s string) bool {
	if s == "" || len(s) > 512 || !utf8.ValidString(s) || strings.ContainsAny(s, "\r\n\t\x00") {
		return false
	}
	u, e := url.Parse(s)
	// A protected reference is an opaque record ID or a credential-free HTTPS
	// location. Query strings/fragments can embed bearer credentials.
	return e == nil && (u.Scheme == "" && !strings.ContainsAny(s, " /\\:@?#") || u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "")
}
func (e RecoveryEvidence) Validate() error {
	if e.Stop.Method != "agent" && e.Stop.Method != "host" && e.Stop.Method != "operator" || !boundedStatement(e.Stop.Statement) || !protectedReference(e.Stop.Reference) || !boundedStatement(e.Reconciliation.Statement) || e.Reconciliation.References == nil || len(e.Reconciliation.References) > 16 {
		return Fail("INVALID_REQUEST")
	}
	for _, r := range e.Reconciliation.References {
		if !protectedReference(r) {
			return Fail("INVALID_REQUEST")
		}
	}
	return nil
}

func (s ExecutionSpec) Provenance() Provenance {
	if s.Recovery != nil {
		return *s.Recovery
	}
	return Provenance{Kind: "normal"}
}
