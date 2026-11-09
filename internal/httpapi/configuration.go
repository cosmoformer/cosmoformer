package httpapi

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/cryptostore"
	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

type EnabledInput struct {
	OrgIDPath
	Body struct {
		Revision int64 `json:"expected_revision" minimum:"1"`
		Enabled  bool  `json:"enabled"`
	}
}

type RuntimeInput struct {
	Body struct {
		Engine       string `json:"engine" enum:"opentofu,terraform"`
		Version      string `json:"version" pattern:"^[0-9]+\\.[0-9]+\\.[0-9]+$"`
		Architecture string `json:"architecture" enum:"amd64"`
		ImageDigest  string `json:"image_digest" pattern:"^sha256:[0-9a-f]{64}$"`
		Approved     bool   `json:"approved"`
	}
}
type IntegrationInput struct {
	OrgPath
	Body struct {
		Name string `json:"name" minLength:"1" maxLength:"100"`
	}
}
type RepositoryInput struct {
	OrgPath
	Body struct {
		ProjectID     string `json:"project_id" format:"uuid"`
		IntegrationID string `json:"integration_id" format:"uuid"`
		ExternalID    string `json:"external_id" minLength:"1" maxLength:"128"`
		URL           string `json:"url"`
		Branch        string `json:"tracked_branch"`
		Enabled       bool   `json:"enabled"`
	}
}
type PoolInput struct {
	OrgPath
	Body struct {
		Name string `json:"name" minLength:"1" maxLength:"100"`
	}
}
type WorkerInput struct {
	OrgPath
	Body struct {
		Name   string   `json:"name" minLength:"1" maxLength:"100"`
		PoolID string   `json:"pool_id" format:"uuid"`
		Tags   []string `json:"tags" maxItems:"16" uniqueItems:"true"`
	}
}
type WorkerTokenOutput struct {
	Body struct {
		ID     string `json:"id" format:"uuid"`
		Token  string `json:"token"`
		Paused bool   `json:"paused"`
	}
}
type RevisionInput struct {
	OrgIDPath
	Body struct {
		Revision int64 `json:"expected_revision" minimum:"1"`
	}
}
type ResourceGrantInput struct {
	OrgPath
	Body struct {
		ProjectID     string `json:"project_id" format:"uuid"`
		EnvironmentID string `json:"environment_id,omitempty" format:"uuid"`
		Kind          string `json:"kind" enum:"repository,pool,secret,s3_object,ddb_table"`
		ResourceID    string `json:"resource_id,omitempty" format:"uuid"`
		Namespace     string `json:"namespace,omitempty" maxLength:"2048"`
		Key           string `json:"resource_key,omitempty" maxLength:"2048"`
	}
}
type AWSInput struct {
	OrgPath
	Body struct {
		ProjectID      string `json:"project_id" format:"uuid"`
		EnvironmentID  string `json:"environment_id,omitempty" format:"uuid"`
		RoleARN        string `json:"role_arn"`
		Bootstrap      string `json:"expected_bootstrap_principal,omitempty"`
		SessionSeconds *int64 `json:"session_seconds,omitempty"`
		Account        string `json:"expected_account" pattern:"^[0-9]{12}$"`
	}
}
type SecretInput struct {
	OrgPath
	Body struct {
		ProjectID string `json:"project_id,omitempty" format:"uuid"`
		Name      string `json:"name" minLength:"1" maxLength:"100"`
		Value     string `json:"value" minLength:"1" maxLength:"16384"`
	}
}
type CredentialInput struct {
	OrgIDPath
	Body struct {
		Revision int64  `json:"expected_revision" minimum:"0"`
		Username string `json:"username" minLength:"1" maxLength:"100"`
		Token    string `json:"token" minLength:"1" maxLength:"16384"`
	}
}
type SecretReplaceInput struct {
	OrgIDPath
	Body struct {
		Revision int64   `json:"expected_revision" minimum:"1"`
		Enabled  bool    `json:"enabled"`
		Value    *string `json:"value,omitempty" minLength:"1" maxLength:"16384"`
	}
}
type Metadata struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Revision int64  `json:"revision"`
	Kind     string `json:"kind"`
	Enabled  bool   `json:"enabled"`
}
type ConfigurationOutput struct {
	Body struct {
		Items []Metadata    `json:"items"`
		Jobs  []JobMetadata `json:"jobs"`
	}
}
type JobMetadata struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
	Revision int64  `json:"revision"`
}

func (a *Server) adminGuard(ctx context.Context, org string) func(pgx.Tx, store.Actor) error {
	return func(tx pgx.Tx, actor store.Actor) error { return orgAdmin(ctx, tx, actor, org) }
}
func (a *Server) seal(ctx context.Context, org, id, purpose string, value []byte) ([]byte, error) {
	return a.sealRevision(ctx, org, id, purpose, 1, value)
}
func (a *Server) sealRevision(ctx context.Context, org, id, purpose string, revision int64, value []byte) ([]byte, error) {
	if a.Cipher == nil {
		return nil, problem(ctx, 503, "Managed credentials require an operator-configured encryption key.")
	}
	return a.Cipher.Seal(value, cryptostore.AAD(org, id, purpose, uint64(revision)), func(f [32]byte) error {
		return a.Quota.Take(f, func(f [32]byte, n int64) error {
			_, e := a.Store.Pool.Exec(ctx, `SELECT cosmoformer.reserve_encryptions($1,$2)`, f[:], n)
			return e
		})
	})
}
func (a *Server) registerConfiguration(api huma.API) {
	register(api, "PATCH", "/organizations/{org}/secrets/{id}", "replace-secret", func(ctx context.Context, in *SecretReplaceInput) (*OK, error) {
		var project string
		var semantic, revision int64
		var ciphertext []byte
		authorize := func(tx pgx.Tx, actor store.Actor) error {
			if e := tx.QueryRow(ctx, `SELECT coalesce(project_id::text,''),semantic_revision,revision,encrypted_value FROM cosmoformer.secrets WHERE id=$1`, in.ID).Scan(&project, &semantic, &revision, &ciphertext); e != nil {
				return store.ErrNotFound
			}
			return manager(ctx, tx, actor, in.Org, project, "")
		}
		if e := a.tx(ctx, in.Org, authorize); e != nil {
			return nil, e
		}
		if revision != in.Body.Revision || a.Cipher == nil {
			return nil, problem(ctx, 409, "Secret settings changed or the encryption key is unavailable.")
		}
		var plain []byte
		var e error
		if in.Body.Value != nil {
			plain = []byte(*in.Body.Value)
		} else {
			plain, e = a.Cipher.Open(ciphertext, cryptostore.AAD(in.Org, in.ID, "managed_secret", uint64(semantic)))
			if e != nil {
				return nil, fail(ctx, e)
			}
		}
		if len(plain) == 0 || len(plain) > 16384 || strings.ContainsRune(string(plain), 0) {
			return nil, problem(ctx, 422, "Secret must be nonempty text of at most 16 KiB without NUL.")
		}
		newSemantic := semantic + 1
		sealed, e := a.sealRevision(ctx, in.Org, in.ID, "managed_secret", newSemantic, plain)
		if e != nil {
			return nil, fail(ctx, e)
		}
		return mutate(a, ctx, in.Org, true, authorize, func(tx pgx.Tx, actor store.Actor) (*OK, error) {
			tag, e := tx.Exec(ctx, `UPDATE cosmoformer.secrets SET encrypted_value=$3,semantic_revision=$4,revision=revision+1,enabled=$5 WHERE id=$1 AND revision=$2`, in.ID, in.Body.Revision, sealed, newSemantic, in.Body.Enabled)
			if e != nil {
				return nil, e
			}
			if tag.RowsAffected() != 1 {
				return nil, store.ErrConflict
			}
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.plans SET status='invalidated',invalidation_reason='SECRET_CHANGED',revision=revision+1 WHERE status='ready'`); e != nil {
				return nil, e
			}
			return ok(), store.Audit(ctx, tx, in.Org, actor.ID, "secret.replace", in.ID)
		})
	})
	for _, resource := range []struct{ Path, Table string }{{"resource-grants", "resource_grants"}, {"worker-pools", "worker_pools"}, {"repositories", "repositories"}, {"aws-connections", "aws_connections"}, {"integrations", "integrations"}} {
		register(api, "PATCH", "/organizations/{org}/"+resource.Path+"/{id}", "toggle-"+resource.Path, func(ctx context.Context, in *EnabledInput) (*OK, error) {
			return mutate(a, ctx, in.Org, false, a.adminGuard(ctx, in.Org), func(tx pgx.Tx, actor store.Actor) (*OK, error) {
				tag, e := tx.Exec(ctx, `UPDATE cosmoformer.`+resource.Table+` SET enabled=$3,revision=revision+1 WHERE id=$1 AND revision=$2`, in.ID, in.Body.Revision, in.Body.Enabled)
				if e != nil {
					return nil, e
				}
				if tag.RowsAffected() != 1 {
					return nil, store.ErrConflict
				}
				if _, e = tx.Exec(ctx, `UPDATE cosmoformer.plans SET status='invalidated',invalidation_reason='CONFIGURATION_CHANGED',revision=revision+1 WHERE status='ready'`); e != nil {
					return nil, e
				}
				return ok(), store.Audit(ctx, tx, in.Org, actor.ID, resource.Path+".update", in.ID)
			})
		})
	}
	register(api, "POST", "/admin/runtimes", "create-runtime", func(ctx context.Context, in *RuntimeInput) (*IDResult, error) {
		return mutate(a, ctx, "", false, installGate, func(tx pgx.Tx, actor store.Actor) (*IDResult, error) {
			var id string
			e := tx.QueryRow(ctx, `INSERT INTO cosmoformer.runtime_catalog(engine,version,architecture,image_digest,approved) VALUES($1,$2,$3,$4,$5) RETURNING id::text`, in.Body.Engine, in.Body.Version, in.Body.Architecture, in.Body.ImageDigest, in.Body.Approved).Scan(&id)
			return idResult(id), e
		})
	})
	register(api, "POST", "/organizations/{org}/integrations", "create-integration", func(ctx context.Context, in *IntegrationInput) (*IDResult, error) {
		return mutate(a, ctx, in.Org, false, a.adminGuard(ctx, in.Org), func(tx pgx.Tx, actor store.Actor) (*IDResult, error) {
			var id string
			e := tx.QueryRow(ctx, `INSERT INTO cosmoformer.integrations(org_id,name,provider,enabled) VALUES($1,$2,'bitbucket_cloud',true) RETURNING id::text`, in.Org, in.Body.Name).Scan(&id)
			return idResult(id), e
		})
	})
	register(api, "POST", "/organizations/{org}/repositories", "create-repository", func(ctx context.Context, in *RepositoryInput) (*IDResult, error) {
		if !source.Repository(in.Body.URL) || !catalog.Branch(in.Body.Branch) {
			return nil, problem(ctx, 422, "Use an exact Bitbucket HTTPS repository and branch.")
		}
		return mutate(a, ctx, in.Org, false, a.adminGuard(ctx, in.Org), func(tx pgx.Tx, actor store.Actor) (*IDResult, error) {
			var id string
			e := tx.QueryRow(ctx, `INSERT INTO cosmoformer.repositories(org_id,project_id,integration_id,external_id,url,tracked_branch,enabled) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id::text`, in.Org, in.Body.ProjectID, in.Body.IntegrationID, in.Body.ExternalID, in.Body.URL, in.Body.Branch, in.Body.Enabled).Scan(&id)
			return idResult(id), e
		})
	})
	register(api, "POST", "/organizations/{org}/worker-pools", "create-worker-pool", func(ctx context.Context, in *PoolInput) (*IDResult, error) {
		return mutate(a, ctx, in.Org, false, a.adminGuard(ctx, in.Org), func(tx pgx.Tx, actor store.Actor) (*IDResult, error) {
			var id string
			e := tx.QueryRow(ctx, `INSERT INTO cosmoformer.worker_pools(org_id,name) VALUES($1,$2) RETURNING id::text`, in.Org, in.Body.Name).Scan(&id)
			return idResult(id), e
		})
	})
	register(api, "POST", "/organizations/{org}/workers", "create-worker", func(ctx context.Context, in *WorkerInput) (*WorkerTokenOutput, error) {
		for _, tag := range in.Body.Tags {
			if !regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`).MatchString(tag) {
				return nil, problem(ctx, 422, "Invalid worker tag.")
			}
		}
		return mutate(a, ctx, in.Org, true, a.adminGuard(ctx, in.Org), func(tx pgx.Tx, actor store.Actor) (*WorkerTokenOutput, error) {
			out := &WorkerTokenOutput{}
			out.Body.Token = identity.Token()
			out.Body.Paused = true
			e := tx.QueryRow(ctx, `INSERT INTO cosmoformer.workers(org_id,pool_id,name,verifier,tags) VALUES($1,$2,$3,$4,$5) RETURNING id::text`, in.Org, in.Body.PoolID, in.Body.Name, identity.Digest(out.Body.Token), in.Body.Tags).Scan(&out.Body.ID)
			if e == nil {
				e = store.Audit(ctx, tx, in.Org, actor.ID, "worker.enroll", out.Body.ID)
			}
			return out, e
		})
	})
	for _, action := range []string{"pause", "resume", "credentials"} {
		register(api, "POST", "/organizations/{org}/workers/{id}/"+action, "worker-admin-"+action, func(ctx context.Context, in *RevisionInput) (*WorkerTokenOutput, error) {
			return mutate(a, ctx, in.Org, action == "credentials", a.adminGuard(ctx, in.Org), func(tx pgx.Tx, actor store.Actor) (*WorkerTokenOutput, error) {
				out := &WorkerTokenOutput{}
				out.Body.ID = in.ID
				var changed bool
				if action == "credentials" {
					out.Body.Token = identity.Token()
					e := tx.QueryRow(ctx, `UPDATE cosmoformer.workers SET verifier=$4,paused=true,revision=revision+1 WHERE org_id=$1 AND id=$2 AND revision=$3 RETURNING paused`, in.Org, in.ID, in.Body.Revision, identity.Digest(out.Body.Token)).Scan(&out.Body.Paused)
					if e != nil {
						return nil, store.ErrConflict
					}
				} else {
					out.Body.Paused = action == "pause"
					e := tx.QueryRow(ctx, `WITH u AS (UPDATE cosmoformer.workers SET paused=$4,revision=revision+1 WHERE org_id=$1 AND id=$2 AND revision=$3 RETURNING id) SELECT EXISTS(SELECT FROM u)`, in.Org, in.ID, in.Body.Revision, out.Body.Paused).Scan(&changed)
					if e != nil {
						return nil, e
					}
					if !changed {
						return nil, store.ErrConflict
					}
				}
				return out, store.Audit(ctx, tx, in.Org, actor.ID, "worker."+action, in.ID)
			})
		})
	}
	register(api, "POST", "/organizations/{org}/resource-grants", "create-resource-grant", func(ctx context.Context, in *ResourceGrantInput) (*IDResult, error) {
		b := in.Body
		if b.Kind == "s3_object" || b.Kind == "ddb_table" {
			if b.ResourceID != "" || b.Namespace == "" || (b.Kind == "s3_object" && b.Key == "") {
				return nil, problem(ctx, 422, "An exact resource namespace is required.")
			}
		} else if !catalog.UUID(b.ResourceID) || b.Namespace != "" || b.Key != "" {
			return nil, problem(ctx, 422, "An exact resource identifier is required.")
		}
		return mutate(a, ctx, in.Org, false, a.adminGuard(ctx, in.Org), func(tx pgx.Tx, actor store.Actor) (*IDResult, error) {
			if b.ResourceID != "" {
				table := map[string]string{"repository": "repositories", "pool": "worker_pools", "secret": "secrets"}[b.Kind]
				var exists bool
				if e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.`+table+` WHERE id=$1)`, b.ResourceID).Scan(&exists); e != nil {
					return nil, e
				}
				if !exists {
					return nil, store.ErrNotFound
				}
			}
			var id string
			e := tx.QueryRow(ctx, `INSERT INTO cosmoformer.resource_grants(org_id,project_id,environment_id,kind,resource_id,namespace,resource_key) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id::text`, in.Org, b.ProjectID, nilString(b.EnvironmentID), b.Kind, nilString(b.ResourceID), nilString(b.Namespace), nilString(b.Key)).Scan(&id)
			return idResult(id), e
		})
	})
	register(api, "POST", "/organizations/{org}/aws-connections", "create-aws-reference", func(ctx context.Context, in *AWSInput) (*IDResult, error) {
		if !regexp.MustCompile(`^arn:aws(-cn|-us-gov)?:iam::` + in.Body.Account + `:role/[A-Za-z0-9_+=,.@/-]+$`).MatchString(in.Body.RoleARN) {
			return nil, problem(ctx, 422, "Use a role ARN in the expected account.")
		}
		if (in.Body.Bootstrap == "") != (in.Body.SessionSeconds == nil) || in.Body.SessionSeconds != nil && (*in.Body.SessionSeconds != 3600 || !regexp.MustCompile(`^arn:aws(-cn|-us-gov)?:iam::[0-9]{12}:role/[A-Za-z0-9_+=,.@/-]+$`).MatchString(in.Body.Bootstrap)) {
			return nil, problem(ctx, 422, "Execution metadata requires an exact bootstrap role and a fixed 3600-second session.")
		}
		return mutate(a, ctx, in.Org, false, a.adminGuard(ctx, in.Org), func(tx pgx.Tx, actor store.Actor) (*IDResult, error) {
			var id string
			e := tx.QueryRow(ctx, `INSERT INTO cosmoformer.aws_connections(org_id,project_id,environment_id,role_arn,expected_account,expected_bootstrap_principal,session_seconds,enabled) VALUES($1,$2,$3,$4,$5,$6,$7,true) RETURNING id::text`, in.Org, in.Body.ProjectID, nilString(in.Body.EnvironmentID), in.Body.RoleARN, in.Body.Account, nilString(in.Body.Bootstrap), in.Body.SessionSeconds).Scan(&id)
			return idResult(id), e
		})
	})
	register(api, "POST", "/organizations/{org}/secrets", "create-secret", func(ctx context.Context, in *SecretInput) (*IDResult, error) {
		if len(in.Body.Value) > 16384 || strings.ContainsRune(in.Body.Value, 0) {
			return nil, problem(ctx, 422, "Secret must contain nonempty UTF-8 text without NUL.")
		}
		if e := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			return manager(ctx, tx, actor, in.Org, in.Body.ProjectID, "")
		}); e != nil {
			return nil, e
		}
		id := runs.ID()
		sealed, e := a.seal(ctx, in.Org, id, "managed_secret", []byte(in.Body.Value))
		if e != nil {
			return nil, fail(ctx, e)
		}
		return mutate(a, ctx, in.Org, true, func(tx pgx.Tx, actor store.Actor) error {
			return manager(ctx, tx, actor, in.Org, in.Body.ProjectID, "")
		}, func(tx pgx.Tx, actor store.Actor) (*IDResult, error) {
			_, e := tx.Exec(ctx, `INSERT INTO cosmoformer.secrets(id,org_id,project_id,name,encrypted_value) VALUES($1,$2,$3,$4,$5)`, id, in.Org, nilString(in.Body.ProjectID), in.Body.Name, sealed)
			return idResult(id), e
		})
	})
	register(api, "POST", "/organizations/{org}/repositories/{id}/source-credentials", "create-source-credential", func(ctx context.Context, in *CredentialInput) (*IDResult, error) {
		if len(in.Body.Token) > 16384 || strings.ContainsAny(in.Body.Username+in.Body.Token, "\r\n\x00") {
			return nil, problem(ctx, 422, "Repository credentials must be bounded text without line breaks or NUL.")
		}
		if e := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error { return orgAdmin(ctx, tx, actor, in.Org) }); e != nil {
			return nil, e
		}
		id := runs.ID()
		plain, _ := json.Marshal(in.Body)
		sealed, e := a.seal(ctx, in.Org, id, "source_credential", plain)
		if e != nil {
			return nil, fail(ctx, e)
		}
		return mutate(a, ctx, in.Org, true, a.adminGuard(ctx, in.Org), func(tx pgx.Tx, actor store.Actor) (*IDResult, error) {
			var current int64
			e := tx.QueryRow(ctx, `SELECT revision FROM cosmoformer.source_credentials WHERE repository_id=$1 AND enabled`, in.ID).Scan(&current)
			if e != nil && e != pgx.ErrNoRows {
				return nil, e
			}
			if current != in.Body.Revision {
				return nil, store.ErrConflict
			}
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.source_credentials SET enabled=false,revision=revision+1 WHERE repository_id=$1 AND enabled`, in.ID); e != nil {
				return nil, e
			}
			_, e = tx.Exec(ctx, `INSERT INTO cosmoformer.source_credentials(id,org_id,repository_id,encrypted_value,revision) VALUES($1,$2,$3,$4,$5)`, id, in.Org, in.ID, sealed, current+1)
			return idResult(id), e
		})
	})
	register(api, "GET", "/organizations/{org}/configuration", "configuration", func(ctx context.Context, in *OrgPath) (*ConfigurationOutput, error) {
		out := &ConfigurationOutput{}
		out.Body.Items = []Metadata{}
		out.Body.Jobs = []JobMetadata{}
		e := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			if e := orgAdmin(ctx, tx, actor, in.Org); e != nil {
				return e
			}
			rows, e := tx.Query(ctx, `SELECT id::text,name,revision,'pool',enabled FROM cosmoformer.worker_pools UNION ALL SELECT id::text,name,revision,'worker',NOT paused FROM cosmoformer.workers UNION ALL SELECT id::text,url,revision,'repository',enabled FROM cosmoformer.repositories UNION ALL SELECT id::text,name,revision,'integration',enabled FROM cosmoformer.integrations UNION ALL SELECT id::text,name,revision,'secret',enabled FROM cosmoformer.secrets UNION ALL SELECT id::text,kind,revision,'resource_grant',enabled FROM cosmoformer.resource_grants UNION ALL SELECT id::text,engine||' '||version,revision,'runtime',approved AND NOT withdrawn FROM cosmoformer.runtime_catalog UNION ALL SELECT id::text,role_arn,revision,'aws_connection',enabled FROM cosmoformer.aws_connections UNION ALL SELECT c.id::text,r.url,c.revision,'source_credential',c.enabled FROM cosmoformer.source_credentials c JOIN cosmoformer.repositories r ON r.id=c.repository_id ORDER BY 4,2,1`)
			if e != nil {
				return e
			}
			for rows.Next() {
				var m Metadata
				if e = rows.Scan(&m.ID, &m.Name, &m.Revision, &m.Kind, &m.Enabled); e != nil {
					return e
				}
				out.Body.Items = append(out.Body.Items, m)
			}
			rows.Close()
			if e = rows.Err(); e != nil {
				return e
			}
			jobs, e := tx.Query(ctx, `SELECT id::text,kind,status,attempt_count,revision FROM cosmoformer.outbox WHERE kind IN('source_reconcile','pr_feedback') AND status<>'delivered' ORDER BY next_attempt_at,id LIMIT 100`)
			if e != nil {
				return e
			}
			defer jobs.Close()
			for jobs.Next() {
				var j JobMetadata
				if e = jobs.Scan(&j.ID, &j.Kind, &j.Status, &j.Attempts, &j.Revision); e != nil {
					return e
				}
				out.Body.Jobs = append(out.Body.Jobs, j)
			}
			return jobs.Err()
		})
		return out, e
	})
	a.registerIntegration(api)
	register(api, "POST", "/organizations/{org}/source-jobs/{id}/retry", "retry-source-job", func(ctx context.Context, in *RevisionInput) (*OK, error) {
		return mutate(a, ctx, in.Org, false, a.adminGuard(ctx, in.Org), func(tx pgx.Tx, actor store.Actor) (*OK, error) {
			tag, e := tx.Exec(ctx, `UPDATE cosmoformer.outbox SET status='pending',attempt_count=0,next_attempt_at=clock_timestamp(),revision=revision+1 WHERE id=$1 AND revision=$2 AND status='failed' AND kind IN('source_reconcile','pr_feedback')`, in.ID, in.Body.Revision)
			if e != nil {
				return nil, e
			}
			if tag.RowsAffected() != 1 {
				return nil, store.ErrConflict
			}
			return ok(), store.Audit(ctx, tx, in.Org, actor.ID, "source-job.retry", in.ID)
		})
	})
	a.registerSourceActions(api)
}
func nilString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
