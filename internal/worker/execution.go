package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cosmoformer/cosmoformer/internal/clock"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
)

// FetchExecutionInputs releases values only after the attempt's shared redactor
// accepts every supplied value and JSON scalar leaf. It starts no child.
func (a *Agent) FetchExecutionInputs(ctx context.Context, assignment runs.Assignment, registry *evidence.Registry) ([]runs.EnvSecret, error) {
	if assignment.Phase != "execution" || assignment.Execution == nil || assignment.Execution.Validate() != nil || registry == nil || runs.Digest(assignment.Execution) != assignment.Digest {
		return nil, ErrAgent
	}
	s := assignment.Execution
	request := runs.ExecutionInputs{Identity: a.Identity, RequestID: runs.ID(), SpecDigest: assignment.Digest, Secrets: s.ExpectedSecrets()}
	var out runs.ExecutionInputValues
	if e := a.call(ctx, "/attempts/"+assignment.AttemptID+"/execution-inputs", request, &out); e != nil {
		return nil, e
	}
	if out.SpecDigest != assignment.Digest || len(out.Secrets) != len(s.Secrets) {
		return nil, ErrAgent
	}
	terms := []string{}
	total := 0
	for i, value := range out.Secrets {
		if value.ExpectedSecret != request.Secrets[i] || value.Value == "" || len(value.Value) > 16384 || !utf8.ValidString(value.Value) || strings.ContainsRune(value.Value, 0) {
			return nil, ErrAgent
		}
		total += len(value.Destination) + len(value.Value) + 2
		if total > 131072 {
			return nil, ErrAgent
		}
		terms = append(terms, value.Value)
		if strings.HasPrefix(value.Destination, "TF_VAR_") {
			d := json.NewDecoder(bytes.NewBufferString(value.Value))
			d.UseNumber()
			var v any
			if d.Decode(&v) == nil {
				var leaves func(any)
				leaves = func(v any) {
					switch x := v.(type) {
					case string:
						if x != "" {
							terms = append(terms, x)
						}
					case json.Number:
						canonical, e := canonicalSecretNumber(string(x))
						if e != nil {
							terms = append(terms, "")
							return
						}
						terms = append(terms, string(x), canonical)
					case bool:
						if x {
							terms = append(terms, "true")
						} else {
							terms = append(terms, "false")
						}
					case []any:
						for _, v := range x {
							leaves(v)
						}
					case map[string]any:
						for _, v := range x {
							leaves(v)
						}
					}
				}
				leaves(v)
			}
		}
	}
	if e := registry.Register(terms...); e != nil {
		return nil, ErrAgent
	}
	return out.Secrets, nil
}

// LaunchPermission is the single-use, request-anchored launch/discard boundary.
// The native execution adapter will perform exec after Claim; no executor lives
// here. A claimed permission can never become a never-started attestation.
type LaunchPermission struct {
	mu         sync.Mutex
	identity   runs.Identity
	attempt    string
	permission runs.Permission
	deadline   time.Duration
	state      string
	anchor     time.Duration
}

func NewLaunchPermission(id runs.Identity, attempt string, p runs.Permission, requestAnchor, attemptDeadline time.Duration) (*LaunchPermission, error) {
	duration := clock.Conservative(p.RemainingMS)
	if p.ID == "" || !runs.ValidDigest(p.Digest) || p.Ordinal < 1 || p.Platform == "" || duration <= 0 || requestAnchor < 0 || attemptDeadline <= requestAnchor {
		return nil, ErrAgent
	}
	return &LaunchPermission{identity: id, attempt: attempt, permission: p, deadline: min(attemptDeadline, requestAnchor+duration), state: "not_spawned", anchor: requestAnchor}, nil
}
func (p *LaunchPermission) Replay(reply runs.Permission, requestAnchor time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if reply.ID != p.permission.ID || reply.Ordinal != p.permission.Ordinal || reply.Digest != p.permission.Digest || reply.Platform != p.permission.Platform || !reply.Expires.Equal(p.permission.Expires) || requestAnchor < p.anchor {
		return ErrAgent
	}
	p.deadline = min(p.deadline, requestAnchor+clock.Conservative(reply.RemainingMS))
	return nil
}
func (p *LaunchPermission) Claim(id runs.Identity, attempt, digest string, now time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.identity != id || p.attempt != attempt || p.permission.Digest != digest || p.state != "not_spawned" || now < p.anchor || now >= p.deadline {
		return ErrAgent
	}
	p.state = "spawn_claimed"
	return nil
}
func (p *LaunchPermission) DiscardExpired(id runs.Identity, attempt string, now time.Duration) (runs.CommandEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.identity != id || p.attempt != attempt || p.state != "not_spawned" || now < p.deadline {
		return runs.CommandEvent{}, ErrAgent
	}
	p.state = "discarded"
	return runs.CommandEvent{Ordinal: p.permission.Ordinal, Permission: p.permission.ID, Event: "not_executed", ProofReason: "local_deadline_expired_discarded"}, nil
}

// Canonical decimal strings match native scalar analysis without float loss.
// Oversized expansion fails preparation instead of weakening secret masking.
func canonicalSecretNumber(value string) (string, error) {
	negative := strings.HasPrefix(value, "-")
	s := strings.TrimPrefix(value, "-")
	exponent := 0
	if at := strings.IndexAny(s, "eE"); at >= 0 {
		var e error
		exponent, e = strconv.Atoi(s[at+1:])
		if e != nil || exponent < -16384 || exponent > 16384 {
			return "", ErrAgent
		}
		s = s[:at]
	}
	decimal := strings.IndexByte(s, '.')
	if decimal < 0 {
		decimal = len(s)
	}
	digits := strings.ReplaceAll(s, ".", "")
	position := decimal + exponent
	if position < 0 {
		digits = "0." + strings.Repeat("0", -position) + digits
	} else if position >= len(digits) {
		digits += strings.Repeat("0", position-len(digits))
	} else {
		digits = digits[:position] + "." + digits[position:]
	}
	halves := strings.SplitN(digits, ".", 2)
	integer := strings.TrimLeft(halves[0], "0")
	if integer == "" {
		integer = "0"
	}
	result := integer
	if len(halves) == 2 {
		fraction := strings.TrimRight(halves[1], "0")
		if fraction != "" {
			result += "." + fraction
		}
	}
	if result != "0" && negative {
		result = "-" + result
	}
	if len(result) > 16384 {
		return "", ErrAgent
	}
	return result, nil
}
