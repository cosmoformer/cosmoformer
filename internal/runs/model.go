// Package runs defines the versioned, non-secret worker protocol.
package runs

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/source"
)

func ID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic("random source unavailable")
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

type Source struct {
	RepositoryID  string `json:"repository_id"`
	URL           string `json:"canonical_https_url"`
	Commit        string `json:"full_commit"`
	Root          string `json:"root"`
	SubjectID     string `json:"subject_id"`
	FactsRevision int64  `json:"facts_revision"`
}
type Runtime struct {
	ID          string `json:"runtime_id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Arch        string `json:"arch"`
	ImageDigest string `json:"image_digest"`
}
type Pool struct {
	ID            string   `json:"id"`
	Tags          []string `json:"tags"`
	GrantRevision int64    `json:"grant_revision"`
}
type ResourceRef struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	ResourceID string `json:"resource_id,omitempty"`
	Revision   int64  `json:"revision"`
	Namespace  string `json:"namespace,omitempty"`
	Key        string `json:"resource_key,omitempty"`
}
type SecretRef struct {
	ID          string `json:"id"`
	Scope       string `json:"scope"`
	Revision    int64  `json:"value_revision"`
	Destination string `json:"destination"`
}
type SourceCredential struct {
	ID           string `json:"id"`
	RepositoryID string `json:"repository_id"`
	Revision     int64  `json:"value_revision"`
}
type ReviewPolicy struct {
	Minimum      int   `json:"effective_minimum"`
	Revision     int64 `json:"revision"`
	AllowPRApply bool  `json:"allow_pr_apply"`
}
type AWSRef struct {
	ID       string `json:"connection_id"`
	Revision int64  `json:"revision"`
	RoleARN  string `json:"role_arn"`
	Account  string `json:"expected_account"`
}
type PreflightSpec struct {
	Version               int                `json:"version"`
	Phase                 string             `json:"phase"`
	StackRevisionID       string             `json:"stack_revision_id"`
	Source                Source             `json:"source"`
	Engine                Runtime            `json:"engine"`
	Workspace             string             `json:"workspace"`
	BackendOwnerAccountID string             `json:"backend_owner_account_id"`
	VarFiles              []string           `json:"var_files"`
	BackendFiles          []string           `json:"backend_config_files"`
	Pool                  Pool               `json:"pool"`
	Secrets               []SecretRef        `json:"secret_refs"`
	SourceCredentials     []SourceCredential `json:"source_credential_refs"`
	Grants                []ResourceRef      `json:"resource_grant_refs"`
	Review                ReviewPolicy       `json:"review_policy"`
	RuleVersion           string             `json:"rule_version"`
	ProfileID             string             `json:"profile_id"`
	AWS                   AWSRef             `json:"aws_connection_ref"`
	ExpectedBinding       *source.Backend    `json:"expected_binding,omitempty"`
}
type Admission struct {
	Protocol    int      `json:"protocol_version"`
	WorkerID    string   `json:"worker_id"`
	Agent       string   `json:"agent_instance"`
	RuntimeID   string   `json:"runtime_id"`
	Engine      string   `json:"engine"`
	Version     string   `json:"version"`
	Arch        string   `json:"arch"`
	ImageDigest string   `json:"image_digest"`
	GitPackage  string   `json:"git_package_version"`
	GitVersion  string   `json:"git_version"`
	Phases      []string `json:"supported_work_phases"`
	ClockSource string   `json:"clock_source"`
	ClockOK     bool     `json:"clock_ok"`
}
type Identity struct {
	WorkerID   string `json:"worker_id"`
	Generation int64  `json:"generation"`
	Agent      string `json:"agent_instance"`
}
type Poll struct {
	Identity
	RequestID string `json:"request_id"`
	Wait      int    `json:"wait_seconds"`
}
type Assignment struct {
	AttemptID       string         `json:"attempt_id"`
	OperationID     string         `json:"operation_id"`
	WorkItemID      string         `json:"work_item_id"`
	Kind            string         `json:"kind"`
	Phase           string         `json:"phase"`
	RunID           string         `json:"run_id"`
	StackID         string         `json:"stack_id"`
	Source          Source         `json:"source"`
	StackRevisionID string         `json:"stack_revision_id"`
	RuntimeID       string         `json:"runtime_id"`
	CapSeconds      int64          `json:"cap_seconds"`
	Spec            PreflightSpec  `json:"execution_spec"`
	Execution       *ExecutionSpec `json:"-"`
	Digest          string         `json:"execution_spec_digest"`
	RemainingMS     int64          `json:"remaining_attempt_ms"`
	Platform        string         `json:"budget_platform_instance"`
	NextSeq         int64          `json:"next_seq"`
}
type Message struct {
	Protocol  int    `json:"protocol_version"`
	MessageID string `json:"message_id"`
	Identity
	AttemptID string          `json:"attempt_id"`
	Seq       int64           `json:"seq"`
	Payload   json.RawMessage `json:"payload"`
}
type Prepare struct {
	Phase         string          `json:"phase" enum:"preflight"`
	Result        string          `json:"result"`
	Digest        string          `json:"source_digest"`
	Backend       source.Backend  `json:"backend"`
	Modules       []source.Module `json:"module_graph"`
	Reason        string          `json:"reason,omitempty"`
	NoStateChild  bool            `json:"no_state_child_created"`
	Reaped        bool            `json:"source_processes_reaped"`
	EverPermitted bool            `json:"ever_permitted"`
}
type LogFinal struct {
	Stream       string `json:"stream"`
	LastSequence int64  `json:"last_sequence"`
	Complete     bool   `json:"complete"`
	Unknown      bool   `json:"unknown_loss"`
}
type Result struct {
	LogFinal     *LogFinal `json:"log_final,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	Outcome      string    `json:"outcome"`
	Digest       string    `json:"preflight_digest"`
	NoStateChild bool      `json:"no_state_child_created"`
	Reaped       bool      `json:"source_processes_reaped"`
}
type Transition struct {
	Permission         *Permission `json:"-"`
	Accepted           bool        `json:"accepted"`
	NextSeq            int64       `json:"next_seq"`
	AttemptState       string      `json:"attempt_state"`
	OperationStatus    string      `json:"operation_status"`
	Revision           int64       `json:"revision"`
	Ready              bool        `json:"ready"`
	NextCommandOrdinal int         `json:"next_command_ordinal"`
}
type Effective struct {
	Declaration catalog.Stack      `json:"declaration"`
	Runtime     Runtime            `json:"runtime"`
	Grants      []ResourceRef      `json:"grants"`
	Secrets     []SecretRef        `json:"secrets"`
	Credentials []SourceCredential `json:"source_credentials"`
	AWS         AWSRef             `json:"aws"`
}
