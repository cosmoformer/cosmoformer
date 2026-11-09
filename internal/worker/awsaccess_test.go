package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/awsaccess"
	"github.com/cosmoformer/cosmoformer/internal/runs"
)

func TestAWSDeliveryCannotPrepareLaunchOrReleaseAnAttempt(t *testing.T) {
	for _, scenario := range []string{"nil handle", "unprepared handle", "preflight", "missing spec", "changed digest", "wrong runtime"} {
		t.Run(scenario, func(t *testing.T) {
			assignment := inputAssignment(t, 0)
			a := Agent{Identity: runs.Identity{WorkerID: runs.ID(), Agent: runs.ID(), Generation: 1}, Admission: runs.Admission{RuntimeID: assignment.Execution.Engine.ID}}
			assignment.RuntimeID = a.Admission.RuntimeID
			access := &awsaccess.Access{}
			switch scenario {
			case "nil handle":
				access = nil
			case "preflight":
				assignment.Phase = "preflight"
			case "missing spec":
				assignment.Execution = nil
			case "changed digest":
				assignment.Digest = strings.Repeat("b", 64)
			case "wrong runtime":
				assignment.RuntimeID = runs.ID()
			}
			before := runs.Digest(assignment)
			env, e := a.AWSExecutionEnvironment(context.Background(), access, assignment, time.Minute)
			if e != ErrAgent || env != nil {
				t.Fatal("unprepared AWS environment delivered")
			}
			if runs.Digest(assignment) != before {
				t.Fatal("delivery changed assignment ownership")
			}
		})
	}
}
