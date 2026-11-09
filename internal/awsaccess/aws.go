// Package awsaccess prepares fixed, owner-constrained execution sessions. It
// starts no processes and confers no native command or reservation authority.
package awsaccess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/clock"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
)

var ErrAccess = errors.New("AWS execution access unavailable; attempt ownership is unchanged")

const sessionSeconds = 3600
const stopMargin = 5 * time.Minute

// Binding identifies the same live attempt and its original local deadline.
// Replays may reduce Deadline; they cannot change process/platform or extend it.
type Binding struct {
	runs.Identity
	AttemptID, SpecDigest, Platform string
	Deadline                        time.Duration
}

// Access is an in-memory, single-use preparation handle. Keep one per attempt;
// failed preparation is terminal for this handle, including an uncertain STS
// response. Never reconstruct it to retry or renew the same execution stage.
type Access struct {
	mu                                     sync.Mutex
	binding                                Binding
	bootstrap, execution                   role
	region, bucket, owner, policy, session string
	registry                               *evidence.Registry
	clock                                  clock.Source
	wall                                   func() time.Time
	clients                                clients
	used, ready, failed                    bool
	last, anchor, deadline                 time.Duration
	credentials                            aws.Credentials
	backend                                *s3.Client
}

// New is deliberately inert. It copies only validated immutable authority and
// derives the attempt deadline from the poll's send tick, never response arrival.
func New(expectedBootstrap string, assignment runs.Assignment, worker runs.Identity, pollAnchor time.Duration, registry *evidence.Registry) (*Access, error) {
	return newAccess(expectedBootstrap, assignment, worker, pollAnchor, registry, clock.New(), time.Now, productionClients())
}

func newAccess(expected string, a runs.Assignment, worker runs.Identity, pollAnchor time.Duration, registry *evidence.Registry, ticks clock.Source, wall func() time.Time, clients clients) (*Access, error) {
	if a.Phase != "execution" || a.Kind != "plan" || a.Execution == nil || a.Execution.Validate() != nil || runs.Digest(a.Execution) != a.Digest || !catalog.UUID(a.AttemptID) || !catalog.UUID(a.Platform) || !catalog.UUID(worker.WorkerID) || !catalog.UUID(worker.Agent) || worker.Generation < 1 || pollAnchor < 0 || registry == nil || ticks == nil || wall == nil || a.CapSeconds < 30 || a.CapSeconds > sessionSeconds-300 || a.RemainingMS > a.CapSeconds*1000 || clock.Conservative(a.RemainingMS) <= 0 || a.RuntimeID != a.Execution.Engine.ID {
		return nil, ErrAccess
	}
	s := a.Execution
	b, ok := parseRole(expected)
	r, roleOK := parseRole(s.AWS.RoleARN)
	if !ok || !roleOK || expected != s.AWS.Bootstrap || r.account != s.AWS.Account || r.partition != s.Backend.Target.Partition || b.partition != r.partition {
		return nil, ErrAccess
	}
	if kms, _ := s.Backend.Attributes["kms_key_id"].(string); kms != "" {
		return nil, ErrAccess
	}
	// Preserve exact state/auxiliary grant requirements. Current grants and the
	// permanent binding are still independently checked by the coordinator.
	for _, claim := range s.Backend.Claims {
		found := false
		for _, g := range s.Grants {
			if claim.Kind == "s3_object" && g.Kind == "s3_object" && g.Namespace == claim.Namespace && g.Key == claim.Key || claim.Kind == "ddb_item" && g.Kind == "ddb_table" && g.Namespace == claim.Namespace {
				found = true
			}
		}
		if !found {
			return nil, ErrAccess
		}
	}
	p, e := s.Backend.OwnerSessionPolicy()
	if e != nil {
		return nil, ErrAccess
	}
	duration := clock.Conservative(a.RemainingMS)
	if pollAnchor > time.Duration(1<<63-1)-duration {
		return nil, ErrAccess
	}
	return &Access{binding: Binding{worker, a.AttemptID, a.Digest, a.Platform, pollAnchor + duration}, bootstrap: b, execution: r, region: s.Backend.Region, bucket: s.Backend.Target.Bucket, owner: s.Backend.ExpectedOwner, policy: string(p), session: "cosmoformer-" + a.AttemptID, registry: registry, clock: ticks, wall: wall, clients: clients, last: pollAnchor}, nil
}

// Formatting/serialization must never traverse credential-bearing fields.
func (*Access) Format(s fmt.State, _ rune)   { io.WriteString(s, "AWS execution access (protected)") }
func (*Access) MarshalJSON() ([]byte, error) { return nil, ErrAccess }

func (a *Access) now() (time.Duration, error) {
	if a.clock == nil {
		a.failed = true
		return 0, ErrAccess
	}
	n, e := a.clock.Now()
	if e != nil || n < a.last || a.failed {
		a.failed = true
		return 0, ErrAccess
	}
	a.last = n
	return n, nil
}

var accessKey = regexp.MustCompile(`^ASIA[A-Z0-9]{16}$`)
var secretKey = regexp.MustCompile(`^[A-Za-z0-9/+=]{40}$`)

func validCredentials(c aws.Credentials, now time.Time) bool {
	if !accessKey.MatchString(c.AccessKeyID) || !secretKey.MatchString(c.SecretAccessKey) || len(c.SessionToken) == 0 || len(c.SessionToken) > 16384 || !c.CanExpire || !c.Expires.After(now) {
		return false
	}
	for _, c := range c.SessionToken {
		if c < '!' || c > '~' {
			return false
		}
	}
	return true
}
func (a *Access) register(c aws.Credentials) error {
	if !validCredentials(c, a.wall()) || a.registry.Register(c.AccessKeyID, c.SecretAccessKey, c.SessionToken) != nil {
		return ErrAccess
	}
	return nil
}

// Prepare does exactly one AssumeRole with the fixed owner policy. There is no
// credential cache, refresh provider, automatic retry or alternate source.
func (a *Access) Prepare(ctx context.Context) (err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.used || a.clock == nil || a.clients.metadata == nil || a.clients.sts == nil || a.clients.s3 == nil {
		return ErrAccess
	}
	a.used = true
	defer func() {
		if err != nil {
			a.failed = true
			a.credentials = aws.Credentials{}
			a.backend = nil
		}
	}()
	n, e := a.now()
	if e != nil || n >= a.binding.Deadline {
		return ErrAccess
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, e := a.clients.bootstrap(ctx, a.bootstrap)
	if e != nil || a.register(c) != nil {
		return ErrAccess
	}
	bootstrap := a.clients.sts(c, a.region)
	id, e := bootstrap.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if e != nil || id == nil || !a.bootstrap.identity(aws.ToString(id.Arn), aws.ToString(id.Account), aws.ToString(id.UserId), "") {
		return ErrAccess
	}
	// Both elapsed and UTC anchors precede the request. The UTC expiration may
	// only shorten this elapsed budget; wall-clock changes cannot extend it.
	a.anchor, e = a.now()
	if e != nil || a.anchor >= a.binding.Deadline || a.anchor > time.Duration(1<<63-1)-time.Hour || !validCredentials(c, a.wall()) {
		return ErrAccess
	}
	wallStart := a.wall()
	out, e := bootstrap.AssumeRole(ctx, &sts.AssumeRoleInput{RoleArn: aws.String(a.execution.arn), RoleSessionName: aws.String(a.session), DurationSeconds: aws.Int32(sessionSeconds), Policy: aws.String(a.policy)})
	if e != nil || out == nil || out.Credentials == nil || out.AssumedRoleUser == nil {
		return ErrAccess
	}
	v := out.Credentials
	if v.Expiration == nil {
		return ErrAccess
	}
	creds := aws.Credentials{AccessKeyID: aws.ToString(v.AccessKeyId), SecretAccessKey: aws.ToString(v.SecretAccessKey), SessionToken: aws.ToString(v.SessionToken), CanExpire: true, Expires: *v.Expiration}
	if a.register(creds) != nil || !a.execution.identity(aws.ToString(out.AssumedRoleUser.Arn), a.execution.account, aws.ToString(out.AssumedRoleUser.AssumedRoleId), a.session) {
		return ErrAccess
	}
	a.deadline = a.anchor + clock.Conservative(min(int64(sessionSeconds*1000), creds.Expires.Sub(wallStart).Milliseconds())) - stopMargin
	n, e = a.now()
	if e != nil || n >= a.binding.Deadline || n >= a.deadline || a.deadline < a.binding.Deadline {
		return ErrAccess
	}
	// A later UTC check can shorten, including a forward clock correction.
	a.deadline = min(a.deadline, n+clock.Conservative(min(int64(sessionSeconds*1000), creds.Expires.Sub(a.wall()).Milliseconds()))-stopMargin)
	if a.deadline <= n || a.deadline < a.binding.Deadline {
		return ErrAccess
	}
	execution := a.clients.sts(creds, a.region)
	id, e = execution.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if e != nil || id == nil || !a.execution.identity(aws.ToString(id.Arn), aws.ToString(id.Account), aws.ToString(id.UserId), a.session) || aws.ToString(id.Arn) != aws.ToString(out.AssumedRoleUser.Arn) || aws.ToString(id.UserId) != aws.ToString(out.AssumedRoleUser.AssumedRoleId) {
		return ErrAccess
	}
	a.credentials = creds
	a.backend = a.clients.s3(creds, a.region)
	if a.checkBackend(ctx) != nil {
		return ErrAccess
	}
	a.ready = true
	return nil
}

func (a *Access) checkBackend(ctx context.Context) error {
	if _, e := a.checkBudget(); e != nil {
		return ErrAccess
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, e := a.backend.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(a.bucket), ExpectedBucketOwner: aws.String(a.owner)})
	if e != nil || out == nil || aws.ToString(out.BucketRegion) != a.region {
		return ErrAccess
	}
	// The SDK projects the first region header. Conflicting/duplicate headers
	// or an unexpected success status must not become ownership proof.
	response, ok := awsmiddleware.GetRawResponse(out.ResultMetadata).(*smithyhttp.Response)
	if !ok || response.Response == nil || response.StatusCode != 200 || len(response.Header.Values("X-Amz-Bucket-Region")) != 1 {
		return ErrAccess
	}
	_, e = a.checkBudget()
	return e
}

func (a *Access) checkBudget() (time.Duration, error) {
	n, e := a.now()
	if e != nil {
		return 0, ErrAccess
	}
	// Synchronized UTC is only an additional shortening check. Moving it back
	// never moves the original elapsed deadline; moving it forward may deny use.
	a.deadline = min(a.deadline, n+clock.Conservative(min(int64(sessionSeconds*1000), a.credentials.Expires.Sub(a.wall()).Milliseconds()))-stopMargin)
	if n >= a.binding.Deadline || n >= a.deadline || a.deadline < a.binding.Deadline {
		a.failed = true
		return 0, ErrAccess
	}
	return n, nil
}

// Environment releases only the five platform-owned temporary AWS values, after
// rechecking the live binding, remaining budgets and owner/region. The caller
// constructs the rest of the child environment from empty and separately needs
// a current native launch permission. No launch or ownership release occurs here.
func (a *Access) Environment(ctx context.Context, current Binding) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.ready || a.failed || current.Identity != a.binding.Identity || current.AttemptID != a.binding.AttemptID || current.SpecDigest != a.binding.SpecDigest || current.Platform != a.binding.Platform || current.Deadline > a.binding.Deadline || current.Deadline <= 0 {
		a.failed = true
		return nil, ErrAccess
	}
	a.binding.Deadline = current.Deadline
	if a.checkBackend(ctx) != nil {
		a.failed = true
		return nil, ErrAccess
	}
	c := a.credentials
	return []string{"AWS_ACCESS_KEY_ID=" + c.AccessKeyID, "AWS_SECRET_ACCESS_KEY=" + c.SecretAccessKey, "AWS_SESSION_TOKEN=" + c.SessionToken, "AWS_REGION=" + a.region, "AWS_DEFAULT_REGION=" + a.region}, nil
}

// RemainingMS is non-secret preparation metadata, bounded by this same live
// elapsed clock. It never starts another timer or grants native authority.
func (a *Access) RemainingMS() (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.ready {
		return 0, ErrAccess
	}
	n, e := a.checkBudget()
	if e != nil {
		return 0, ErrAccess
	}
	return (a.deadline - n).Milliseconds(), nil
}
