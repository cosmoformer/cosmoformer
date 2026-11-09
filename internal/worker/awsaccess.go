package worker

import (
	"context"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/awsaccess"
	"github.com/cosmoformer/cosmoformer/internal/runs"
)

// AWSExecutionEnvironment bridges a previously prepared, single-use access
// handle to this same live worker/attempt. It is never called by admission or
// RunOne and cannot construct, assume or renew a session, launch a process, or
// release an assignment. The deadline is the agent's original poll-anchored
// deadline, possibly shortened by a replay.
func (a *Agent) AWSExecutionEnvironment(ctx context.Context, access *awsaccess.Access, assignment runs.Assignment, attemptDeadline time.Duration) ([]string, error) {
	if access == nil || assignment.Phase != "execution" || assignment.Execution == nil || assignment.Execution.Validate() != nil || runs.Digest(assignment.Execution) != assignment.Digest || assignment.RuntimeID != a.Admission.RuntimeID {
		return nil, ErrAgent
	}
	env, e := access.Environment(ctx, awsaccess.Binding{Identity: a.Identity, AttemptID: assignment.AttemptID, SpecDigest: assignment.Digest, Platform: assignment.Platform, Deadline: attemptDeadline})
	if e != nil {
		return nil, ErrAgent
	}
	return env, nil
}
