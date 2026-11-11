package runs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/source"
)

// ExecutionSpec is resolved input authority, never a command or environment map.
// PreflightSpec remains a distinct source-only wire type.
type ExecutionSpec struct {
	Recovery          *Provenance        `json:"recovery,omitempty"`
	Version           int                `json:"version"`
	Phase             string             `json:"phase"`
	StackRevisionID   string             `json:"stack_revision_id"`
	Source            Source             `json:"source"`
	Engine            Runtime            `json:"engine"`
	Workspace         string             `json:"workspace"`
	VarFiles          []string           `json:"var_files"`
	BackendFiles      []string           `json:"backend_config_files"`
	Pool              Pool               `json:"pool"`
	Backend           source.Backend     `json:"backend"`
	AWS               ExecutionAWS       `json:"aws"`
	Secrets           []SecretRef        `json:"secret_refs"`
	SourceCredentials []SourceCredential `json:"source_credential_refs"`
	Grants            []ResourceRef      `json:"resource_grant_refs"`
	Review            ReviewPolicy       `json:"review_policy"`
	RuleVersion       string             `json:"rule_version"`
	ProfileID         string             `json:"profile_id"`
	RetainedPlan      *RetainedPlan      `json:"retained_plan,omitempty"`
}
type ExecutionAWS struct {
	AWSRef
	Bootstrap      string `json:"expected_bootstrap_principal"`
	SessionSeconds int64  `json:"session_seconds"`
}
type RetainedPlan struct {
	ID             string   `json:"plan_id"`
	Revision       int64    `json:"revision"`
	ManifestDigest string   `json:"manifest_digest"`
	Artifacts      []string `json:"artifact_ids"`
}

func (s ExecutionSpec) Preflight() PreflightSpec {
	return PreflightSpec{Recovery: s.Recovery, Version: s.Version, Phase: "preflight", StackRevisionID: s.StackRevisionID, Source: s.Source, Engine: s.Engine, Workspace: s.Workspace, BackendOwnerAccountID: s.Backend.ExpectedOwner, VarFiles: s.VarFiles, BackendFiles: s.BackendFiles, Pool: s.Pool, Secrets: s.Secrets, SourceCredentials: s.SourceCredentials, Grants: s.Grants, Review: s.Review, RuleVersion: s.RuleVersion, ProfileID: s.ProfileID, AWS: s.AWS.AWSRef, ExpectedBinding: &s.Backend}
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
var commit40 = regexp.MustCompile(`^[0-9a-f]{40}$`)

func ValidDigest(s string) bool { return hex64.MatchString(s) }
func Digest(v any) string {
	b, e := catalog.Canonical(v)
	if e != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (s ExecutionSpec) Validate() error {
	if s.Recovery != nil && (s.Recovery.Kind != "recovery" || !catalog.UUID(s.Recovery.Case) || s.Recovery.Revision < 1) {
		return Fail("INVALID_SPEC")
	}
	if s.Version != 1 || s.Phase != "execution" || !catalog.UUID(s.StackRevisionID) || !catalog.UUID(s.Source.RepositoryID) || !catalog.UUID(s.Source.SubjectID) || !commit40.MatchString(s.Source.Commit) || s.Source.FactsRevision < 1 || !catalog.RepoPath(s.Source.Root) || !catalog.UUID(s.Engine.ID) || s.Engine.Arch != "amd64" || !strings.HasPrefix(s.Engine.ImageDigest, "sha256:") || !ValidDigest(strings.TrimPrefix(s.Engine.ImageDigest, "sha256:")) || !catalog.UUID(s.Pool.ID) || s.Pool.GrantRevision < 1 || s.AWS.SessionSeconds != 3600 || s.AWS.Bootstrap == "" || !catalog.UUID(s.AWS.ID) || s.AWS.Revision < 1 || s.Review.Minimum < 1 || s.Review.Minimum > 100 || s.Review.Revision < 1 || s.RuleVersion == "" || s.ProfileID == "" || s.Workspace == "" {
		return Fail("INVALID_SPEC")
	}
	if (s.Engine.Name != "opentofu" || s.Engine.Version != "1.13.8") && (s.Engine.Name != "terraform" || s.Engine.Version != "1.17.1") {
		return Fail("RUNTIME_UNSUPPORTED")
	}
	if !source.Repository(s.Source.URL) {
		return Fail("INVALID_SPEC")
	}
	if len(s.Secrets) > 64 || s.Secrets == nil || s.Grants == nil || s.SourceCredentials == nil || s.VarFiles == nil || s.BackendFiles == nil || len(s.Grants) > 512 || len(s.SourceCredentials) > 128 || len(s.Pool.Tags) > 16 {
		return Fail("INVALID_SPEC")
	}
	for _, files := range [][]string{s.VarFiles, s.BackendFiles} {
		if len(files) > 32 {
			return Fail("INVALID_SPEC")
		}
		seen := map[string]bool{}
		for _, f := range files {
			if !catalog.RepoPath(f) || seen[f] {
				return Fail("INVALID_SPEC")
			}
			seen[f] = true
		}
	}
	seen := map[string]bool{}
	for _, r := range s.Secrets {
		if !catalog.UUID(r.ID) || r.Revision < 1 || !catalog.SecretEnv(r.Destination) || seen[r.Destination] || (r.Scope != "org" && r.Scope != "project") {
			return Fail("INVALID_SPEC")
		}
		seen[r.Destination] = true
	}
	grantIDs := map[string]bool{}
	poolOK := false
	for _, r := range s.Grants {
		if !catalog.UUID(r.ID) || r.Revision < 1 || grantIDs[r.ID] {
			return Fail("INVALID_SPEC")
		}
		grantIDs[r.ID] = true
		poolOK = poolOK || r.Kind == "pool" && r.ResourceID == s.Pool.ID && r.Revision == s.Pool.GrantRevision
	}
	if !poolOK {
		return Fail("INVALID_SPEC")
	}
	for _, r := range s.SourceCredentials {
		if !catalog.UUID(r.ID) || !catalog.UUID(r.RepositoryID) || r.Revision < 1 {
			return Fail("INVALID_SPEC")
		}
	}
	b, e := source.CanonicalBackend(s.Backend.Attributes, source.BackendContext{Engine: catalog.Engine{Name: s.Engine.Name, Version: s.Engine.Version}, Workspace: s.Workspace, ExpectedOwner: s.Backend.ExpectedOwner}, s.Backend.TableARN)
	if e != nil || Digest(b) != Digest(s.Backend) {
		return Fail("BACKEND_MISMATCH")
	}
	if s.Backend.TableARN != "" {
		parts := strings.Split(s.Backend.TableARN, ":")
		if len(parts) != 6 || parts[4] != s.AWS.Account {
			return Fail("BACKEND_MISMATCH")
		}
	}
	return nil
}

type ExpectedSecret struct {
	ID          string `json:"id"`
	Revision    int64  `json:"value_revision"`
	Destination string `json:"destination"`
}

func (s ExecutionSpec) ExpectedSecrets() []ExpectedSecret {
	out := make([]ExpectedSecret, 0, len(s.Secrets))
	for _, r := range s.Secrets {
		out = append(out, ExpectedSecret{r.ID, r.Revision, r.Destination})
	}
	return out
}

type ExecutionInputs struct {
	Identity
	RequestID  string           `json:"request_id"`
	SpecDigest string           `json:"execution_spec_digest"`
	Secrets    []ExpectedSecret `json:"expected_secret_refs"`
}
type EnvSecret struct {
	ExpectedSecret
	Value string `json:"value"`
}
type ExecutionInputValues struct {
	SpecDigest string      `json:"execution_spec_digest"`
	Secrets    []EnvSecret `json:"env_secrets"`
}

type InputLock struct {
	Origin  string `json:"origin"`
	Present bool   `json:"present"`
	Hash    string `json:"sha256,omitempty"`
}
type ExecutionPrepare struct {
	Phase         string    `json:"phase" enum:"execution"`
	Result        string    `json:"result"`
	Artifact      string    `json:"pre_init_spec_artifact_id"`
	Digest        string    `json:"pre_init_spec_digest"`
	Commit        string    `json:"source_commit"`
	Lock          InputLock `json:"input_lock"`
	SpecDigest    string    `json:"execution_spec_digest"`
	Bootstrap     string    `json:"bootstrap_principal"`
	Role          string    `json:"role_arn"`
	CredentialMS  int64     `json:"credential_remaining_ms"`
	Reason        string    `json:"reason,omitempty"`
	NoStateChild  bool      `json:"no_state_child_created,omitempty"`
	Reaped        bool      `json:"source_processes_reaped,omitempty"`
	EverPermitted bool      `json:"ever_permitted,omitempty"`
}
type ManifestSeal struct {
	PreInitDigest string     `json:"pre_init_spec_digest"`
	Artifact      string     `json:"manifest_artifact_id"`
	Digest        string     `json:"manifest_digest"`
	LockArtifact  string     `json:"provider_lock_artifact_id,omitempty"`
	Providers     []Provider `json:"external_providers"`
}
type CommandRequest struct {
	Ordinal        int64  `json:"ordinal"`
	Command        string `json:"command"`
	Digest         string `json:"command_spec_digest"`
	PreInitDigest  string `json:"pre_init_spec_digest"`
	ManifestDigest string `json:"sealed_manifest_digest,omitempty"`
}
type CommandDescriptor struct {
	Runtime        string   `json:"runtime_id"`
	Ordinal        int64    `json:"ordinal"`
	Command        string   `json:"command"`
	Root           string   `json:"root_repo_path"`
	Workspace      string   `json:"workspace"`
	BackendFiles   []string `json:"ordered_backend_files"`
	VarFiles       []string `json:"ordered_var_files"`
	Output         string   `json:"output_kind"`
	PreInitDigest  string   `json:"pre_init_spec_digest"`
	ManifestDigest string   `json:"sealed_manifest_digest,omitempty"`
}

func Command(s ExecutionSpec, kind string, r CommandRequest) (CommandDescriptor, error) {
	if FixedFlags(r.Command) == nil {
		return CommandDescriptor{}, Fail("COMMAND_UNSUPPORTED")
	}
	commands := []string{"init", "schema", "plan", "show"}
	outputs := []string{"console", "provider_schema", "binary_plan", "raw_plan_json"}
	if kind == "apply" {
		commands = []string{"init", "apply"}
		outputs = []string{"console", "console"}
	} else if kind != "plan" {
		return CommandDescriptor{}, Fail("COMMAND_UNSUPPORTED")
	}
	if r.Ordinal < 1 || r.Ordinal > int64(len(commands)) || r.Command != commands[r.Ordinal-1] {
		return CommandDescriptor{}, Fail("COMMAND_ORDER")
	}
	if !ValidDigest(r.PreInitDigest) || r.Ordinal == 1 && r.ManifestDigest != "" || r.Ordinal > 1 && !ValidDigest(r.ManifestDigest) {
		return CommandDescriptor{}, Fail("MANIFEST_STAGE")
	}
	return CommandDescriptor{s.Engine.ID, r.Ordinal, r.Command, s.Source.Root, s.Workspace, s.BackendFiles, s.VarFiles, outputs[r.Ordinal-1], r.PreInitDigest, r.ManifestDigest}, nil
}

// FixedFlags identifies the immutable F2 templates. No flags are accepted on wire.
func FixedFlags(command string) []string {
	switch command {
	case "init":
		return []string{"init", "-input=false", "-no-color", "-reconfigure", "-lock-timeout=60s"}
	case "schema":
		return []string{"providers", "schema", "-json"}
	case "plan":
		return []string{"plan", "-input=false", "-no-color", "-lock=true", "-lock-timeout=60s", "-detailed-exitcode"}
	case "show":
		return []string{"show", "-json"}
	case "apply":
		return []string{"apply", "-input=false", "-no-color", "-lock=true", "-lock-timeout=60s"}
	}
	return nil
}

type Permission struct {
	ID          string    `json:"permission_id"`
	Ordinal     int64     `json:"ordinal"`
	Digest      string    `json:"command_spec_digest"`
	Platform    string    `json:"platform_instance"`
	RemainingMS int64     `json:"remaining_ms"`
	Expires     time.Time `json:"display_expires_at"`
}
type CommandEvent struct {
	Ordinal     int64  `json:"ordinal"`
	Permission  string `json:"permission_id"`
	Event       string `json:"event"`
	PID         *int64 `json:"pid,omitempty"`
	PGID        *int64 `json:"pgid,omitempty"`
	Exit        *int   `json:"exit_code,omitempty"`
	Signal      *int   `json:"signal,omitempty"`
	GroupEmpty  *bool  `json:"group_empty,omitempty"`
	Pipes       string `json:"pipes,omitempty"`
	ProofReason string `json:"proof_reason,omitempty"`
}
type CommandResult struct {
	Ordinal    int64        `json:"ordinal"`
	Permission string       `json:"permission_id"`
	Status     string       `json:"status"`
	Exit       *int         `json:"exit_code,omitempty"`
	Signal     *int         `json:"signal,omitempty"`
	Stop       CommandEvent `json:"stop_proof"`
}
type ExecutionResult struct {
	Outcome        string          `json:"outcome"`
	Commands       []CommandResult `json:"commands"`
	Artifacts      []string        `json:"artifact_ids"`
	PreInitDigest  string          `json:"pre_init_spec_digest,omitempty"`
	ManifestDigest string          `json:"sealed_manifest_digest,omitempty"`
	LogFinal       *LogFinal       `json:"log_final,omitempty"`
	NoStateChild   bool            `json:"no_state_child_created,omitempty"`
	Reaped         bool            `json:"source_processes_reaped,omitempty"`
	EverPermitted  bool            `json:"ever_permitted,omitempty"`
}

// MessageDigest rejects unknown/secret-bearing payload members before replay
// hashing. Only closed typed non-secret transitions may enter the message log.
func ExecutionPayload(route string, b json.RawMessage) (any, error) {
	var p any
	switch route {
	case "ack":
		p = &struct {
			Work string `json:"work_item_id"`
		}{}
	case "prepare-result":
		p = &ExecutionPrepare{}
	case "manifest-seal":
		p = &ManifestSeal{}
	case "start-permission":
		p = &CommandRequest{}
	case "command-events":
		p = &CommandEvent{}
	case "result":
		p = &ExecutionResult{}
	case "controls/ack":
		p = &ControlAck{}
	default:
		return nil, Fail("COMMAND_UNSUPPORTED")
	}
	if e := Decode(b, p); e != nil {
		return nil, e
	}
	return p, nil
}

type ControlAck struct {
	ID        string `json:"control_id"`
	State     string `json:"state"`
	Ordinal   int64  `json:"command_ordinal"`
	Signal    string `json:"signal_sent,omitempty"`
	StopEvent string `json:"stop_event_id,omitempty" doc:"Worker message_id of the accepted stopped command event."`
}
