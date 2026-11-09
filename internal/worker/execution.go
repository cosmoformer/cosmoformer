package worker

import (
	"context"
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
		known, e := evidence.SecretTerms(value.Value, strings.HasPrefix(value.Destination, "TF_VAR_"))
		if e != nil {
			return nil, ErrAgent
		}
		terms = append(terms, known...)
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

// Kept as a package-local alias for the preparation contract tests.
func canonicalSecretNumber(value string) (string, error) {
	return evidence.CanonicalNumber(value)
}
