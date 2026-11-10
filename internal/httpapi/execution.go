package httpapi

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/cosmoformer/cosmoformer/internal/bitbucket"
	"github.com/cosmoformer/cosmoformer/internal/coordinator"
	"github.com/cosmoformer/cosmoformer/internal/cryptostore"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

type ExecutionInputsInput struct {
	Authorization string `header:"Authorization"`
	ID            string `path:"id" format:"uuid"`
	Body          runs.ExecutionInputs
}
type ExecutionInputsOutput struct{ Body runs.ExecutionInputValues }

func (a *Server) registerExecutionInputs(api huma.API) {
	workerRegister(api, "/attempts/{id}/execution-inputs", "worker-execution-inputs", func(ctx context.Context, in *ExecutionInputsInput) (*ExecutionInputsOutput, error) {
		out := &ExecutionInputsOutput{Body: runs.ExecutionInputValues{SpecDigest: in.Body.SpecDigest, Secrets: []runs.EnvSecret{}}}
		e := a.Coordinator.ExecutionInputs(ctx, bearer(in.Authorization), in.ID, in.Body, func(tx pgx.Tx, w coordinator.Worker, s runs.ExecutionSpec) error {
			if a.Cipher == nil && len(s.Secrets) > 0 {
				return store.ErrForbidden
			}
			total := 0
			for _, r := range s.Secrets {
				var encrypted []byte
				if e := tx.QueryRow(ctx, `SELECT encrypted_value FROM cosmoformer.secrets WHERE id=$1 AND semantic_revision=$2 AND enabled`, r.ID, r.Revision).Scan(&encrypted); e != nil {
					return runs.Fail("INPUTS_CHANGED")
				}
				plain, e := a.Cipher.Open(encrypted, cryptostore.AAD(w.OrgID, r.ID, "managed_secret", uint64(r.Revision)))
				if e != nil {
					return store.ErrForbidden
				}
				// Secret writes store the value itself, never a JSON environment map.
				value := string(plain)
				if len(value) == 0 || len(value) > 16384 || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
					return runs.Fail("SECRET_VALUE_UNSUPPORTED")
				}
				total += len(r.Destination) + len(value) + 2
				if total > 131072 {
					return runs.Fail("CHILD_ENV_TOO_LARGE")
				}
				out.Body.Secrets = append(out.Body.Secrets, runs.EnvSecret{ExpectedSecret: runs.ExpectedSecret{ID: r.ID, Revision: r.Revision, Destination: r.Destination}, Value: value})
			}
			return nil
		})
		if e != nil {
			return nil, workerError(ctx, e)
		}
		return out, nil
	})
}

// Source reads are bounded external I/O between the coordinator's ticket and
// acceptance transactions. This adapter has no AWS or native launch behavior.
func (a *Server) workerSourceObservation(ctx context.Context, org, subject string) (bitbucket.Facts, error) {
	var r RepoFacts
	var kind, key string
	var revision int64
	e := a.serviceTx(ctx, org, func(tx pgx.Tx) error {
		var repo string
		if e := tx.QueryRow(ctx, `SELECT repository_id::text,kind,external_key FROM cosmoformer.source_subjects WHERE id=$1`, subject).Scan(&repo, &kind, &key); e != nil {
			return e
		}
		var e error
		r, e = repoFacts(ctx, tx, repo)
		if e != nil {
			return e
		}
		revision = r.Revision
		return nil
	})
	if e != nil {
		return bitbucket.Facts{}, e
	}
	pr := 0
	if kind == "pr" {
		pr, e = strconv.Atoi(key)
		if e != nil || pr < 1 {
			return bitbucket.Facts{}, runs.Fail("SOURCE_CHANGED")
		}
	} else if kind != "branch" || key != r.Branch {
		return bitbucket.Facts{}, runs.Fail("SOURCE_CHANGED")
	}
	token, e := a.apiToken(ctx, org, r.IntegrationID)
	if e != nil {
		return bitbucket.Facts{}, e
	}
	f, e := a.Bitbucket.Read(ctx, r.URL, r.ExternalID, r.Branch, pr, token)
	if e != nil && !errors.Is(e, bitbucket.ErrContextChanged) {
		return f, e
	}
	f.InventoryRevision = revision
	// Return authoritative ineligible facts too. Acceptance must advance the
	// source revision before the immutable assignment's eligibility is checked.
	return f, e
}

func (a *Server) currentExecutionSecrets(ctx context.Context, tx pgx.Tx, org string, s runs.ExecutionSpec) ([]runs.EnvSecret, error) {
	if org == "" {
		if e := tx.QueryRow(ctx, `SELECT current_setting('app.org_id')`).Scan(&org); e != nil {
			return nil, e
		}
	}
	out := []runs.EnvSecret{}
	if len(s.Secrets) > 0 && a.Cipher == nil {
		return nil, store.ErrForbidden
	}
	for _, r := range s.Secrets {
		var encrypted []byte
		if e := tx.QueryRow(ctx, `SELECT encrypted_value FROM cosmoformer.secrets WHERE id=$1 AND semantic_revision=$2 AND enabled`, r.ID, r.Revision).Scan(&encrypted); e != nil {
			return nil, runs.Fail("INPUTS_CHANGED")
		}
		plain, e := a.Cipher.Open(encrypted, cryptostore.AAD(org, r.ID, "managed_secret", uint64(r.Revision)))
		if e != nil {
			return nil, store.ErrForbidden
		}
		value := string(plain)
		if len(value) == 0 || len(value) > 16384 || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return nil, runs.Fail("SECRET_VALUE_UNSUPPORTED")
		}
		out = append(out, runs.EnvSecret{ExpectedSecret: runs.ExpectedSecret{ID: r.ID, Revision: r.Revision, Destination: r.Destination}, Value: value})
	}
	return out, nil
}
