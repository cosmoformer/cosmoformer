package worker

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/plan"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
)

// planNative owns the one fresh execution directory and never reruns a native
// command. false means ownership is unresolved; the directory must be retained.
func (a *Agent) planNative(ctx, attemptCtx context.Context, assignment runs.Assignment, anchor, deadline time.Duration, dir, tree, helper string, inspected source.Inspection, trees map[string]checkoutTree, send func(string, any) error, last *runs.Transition) (bool, error) {
	s := *assignment.Execution
	result := runs.ExecutionResult{Commands: []runs.CommandResult{}, Artifacts: []string{}}
	logs := a.Logs()
	publishCtx, stopPublish := context.WithCancel(ctx)
	defer stopPublish()
	go logs.Publish(publishCtx, a, assignment.AttemptID)
	finish := func(outcome string) (bool, error) {
		if outcome != "not_executed" && supervisor(attemptCtx).wasRequested() && len(result.Commands) > 0 {
			outcome = "cancelled"
		}
		stopPublish()
		if e := logs.Flush(ctx, a, assignment.AttemptID); e != nil {
			result.LogFinal = nil
		} else {
			result.LogFinal = logs.Final()
		}
		// Nil means the platform records a truthful explicit final gap. Archived
		// segments remain available even when the final acknowledgement was lost.
		if outcome == "failure" && len(result.Commands) == 0 {
			outcome = "preparation_failed"
		}
		result.Outcome = outcome
		result.NoStateChild = len(result.Commands) == 0
		result.Reaped = true
		result.EverPermitted = len(result.Commands) > 0
		err := send("result", result)
		var rejected *Rejected
		if outcome == "success" && errors.As(err, &rejected) && rejected.Status >= 400 && rejected.Status < 500 {
			result.Outcome = "failure"
			err = send("result", result)
		}
		return true, err
	}
	reg := logs.Registry
	secrets, e := a.FetchExecutionInputs(attemptCtx, assignment, reg)
	if e != nil {
		return finish("preparation_failed")
	}
	factory := a.Access
	if factory == nil {
		factory = defaultAccess
	}
	access, e := factory(a.ExpectedBootstrap, assignment, a.Identity, anchor, reg)
	if e != nil || access == nil {
		return finish("preparation_failed")
	}
	if e = access.Prepare(attemptCtx); e != nil {
		return finish("preparation_failed")
	}
	aws, e := a.AWSExecutionEnvironment(attemptCtx, access, assignment, deadline)
	if e != nil {
		return finish("preparation_failed")
	}
	env, e := nativeEnvironment(dir, helper, s, secrets, aws)
	if e != nil {
		return finish("preparation_failed")
	}
	reviewed, e := reviewedInputs(assignment, trees, inspected)
	if e != nil {
		return finish("preparation_failed")
	}
	lockPath := path.Join(s.Source.Root, ".terraform.lock.hcl")
	pre := runs.PreInitSpec{ReviewedInputs: reviewed, Kind: "plan", SpecDigest: assignment.Digest, LockOrigin: "absent"}
	for _, f := range reviewed.Files {
		if f.Repository == s.Source.RepositoryID && f.Commit == s.Source.Commit && f.Path == lockPath {
			pre.LockOrigin = "tracked"
			pre.LockHash = f.Hash
		}
	}
	if pre.Validate(s, "plan", assignment.StackID, assignment.StateEpoch) != nil {
		return finish("preparation_failed")
	}
	retain := func(kind string, v any) (string, error) {
		var raw []byte
		if b, ok := v.([]byte); ok {
			raw = b
		} else {
			var e error
			raw, e = catalog.Canonical(v)
			if e != nil {
				return "", ErrAgent
			}
		}
		id, e := a.RetainEvidence(ctx, assignment.AttemptID, kind, raw)
		if e == nil {
			result.Artifacts = append(result.Artifacts, id)
		}
		return id, e
	}
	preID, e := retain("pre_init_spec", pre)
	if e != nil {
		return finish("preparation_failed")
	}
	remaining, e := access.RemainingMS()
	if e != nil {
		return finish("preparation_failed")
	}
	result.PreInitDigest = runs.Digest(pre)
	if e = send("prepare-result", runs.ExecutionPrepare{Phase: "execution", Result: "success", Artifact: preID, Digest: result.PreInitDigest, Commit: s.Source.Commit, Lock: runs.InputLock{Origin: pre.LockOrigin, Present: pre.LockOrigin != "absent", Hash: pre.LockHash}, SpecDigest: assignment.Digest, Bootstrap: s.AWS.Bootstrap, Role: s.AWS.RoleARN, CredentialMS: remaining}); e != nil {
		// Preparation acceptance may be unknown. No native child exists; exact replay
		// of that same message is already attempted by the ordered sender.
		var rejected *Rejected
		if errors.As(e, &rejected) && rejected.Status >= 400 && rejected.Status < 500 {
			result.PreInitDigest = ""
			return finish("preparation_failed")
		}
		return false, e
	}
	runner := a.Native
	if runner == nil {
		runner = runNative
	}
	var manifest runs.Manifest
	var schema, raw []byte
	savedHash := ""
	for i, command := range []string{"init", "schema", "plan", "show"} {
		allowedGenerated := lockPath
		if i == 0 {
			allowedGenerated = ""
		}
		if e = verifyTrees(reviewed, trees, allowedGenerated, s.Source.RepositoryID); e != nil {
			return finish("failure")
		}
		if i > 1 && verifyNativeLock(filepath.Join(tree, lockPath), manifest) != nil {
			return finish("failure")
		}
		// The access handle is never reconstructed or renewed. Every child receives
		// only values from this same guarded fixed session and fresh environment.
		currentAWS, e := a.AWSExecutionEnvironment(attemptCtx, access, assignment, deadline)
		if e != nil {
			return finish("failure")
		}
		if runs.Digest(currentAWS) != runs.Digest(aws) {
			return finish("failure")
		}
		request := runs.CommandRequest{Ordinal: int64(i + 1), Command: command, PreInitDigest: result.PreInitDigest, ManifestDigest: result.ManifestDigest}
		descriptor, e := runs.Command(s, "plan", request)
		if e != nil {
			return finish("failure")
		}
		request.Digest = runs.Digest(descriptor)
		if command == "show" {
			saved, e := protectedBytes(filepath.Join(dir, "plan.bin"), plan.MaxBytes)
			if e != nil || hash(saved) != savedHash {
				return finish("failure")
			}
		}
		program, root, args, e := nativeArgs(s, dir, tree, command, pre.LockOrigin == "tracked")
		if e != nil {
			return finish("failure")
		}
		var output *os.File
		if command == "schema" || command == "show" {
			output, e = os.OpenFile(filepath.Join(dir, command+".json"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
			if e != nil {
				return finish("failure")
			}
		}
		requestAnchor, e := a.Clock.Now()
		if e != nil {
			if output != nil {
				output.Close()
			}
			return finish("failure")
		}
		if e = send("start-permission", request); e != nil {
			if output != nil {
				output.Close()
			}
			var rejected *Rejected
			if errors.As(e, &rejected) && rejected.Status >= 400 && rejected.Status < 500 {
				return finish("failure")
			}
			return false, e
		}
		if last.Permission == nil {
			if output != nil {
				output.Close()
			}
			return false, ErrAgent
		}
		permission := *last.Permission
		launch, e := NewLaunchPermission(a.Identity, assignment.AttemptID, permission, requestAnchor, deadline)
		if e != nil {
			if output != nil {
				output.Close()
			}
			return false, e
		}
		tick, e := a.Clock.Now()
		if e != nil {
			if output != nil {
				output.Close()
			}
			return false, e
		}
		if e = launch.Check(a.Identity, assignment.AttemptID, request.Digest, tick); e != nil {
			if output != nil {
				output.Close()
			}
			event, discardErr := launch.DiscardExpired(a.Identity, assignment.AttemptID, tick)
			if discardErr != nil {
				return false, e
			}
			if e = send("command-events", event); e != nil {
				return false, e
			}
			result.Commands = append(result.Commands, runs.CommandResult{Ordinal: request.Ordinal, Permission: permission.ID, Status: "not_executed", Stop: event})
			return finish("not_executed")
		}
		event, runErr := runner(attemptCtx, NativeCommand{Start: func(start func() error) error {
			return launch.Spawn(a.Identity, assignment.AttemptID, request.Digest, a.Clock, start)
		}, Program: program, Dir: root, Args: args, Env: env, Descriptor: descriptor, Permission: permission, Output: output, Logs: logs}, func(event runs.CommandEvent) error { return send("command-events", event) })
		if output != nil {
			if e = output.Sync(); e != nil && runErr == nil {
				runErr = e
			}
			output.Close()
		}
		if event.Event == "" {
			// The final exec boundary can lose a race to expiry or a delivered Cancel
			// after the earlier Check. Only the still-unclaimed local launch state may
			// provide never-started proof; a claimed/ambiguous spawn stays unresolved.
			var discarded runs.CommandEvent
			discardErr := ErrAgent
			if now, clockErr := a.Clock.Now(); clockErr == nil {
				discarded, discardErr = launch.DiscardExpired(a.Identity, assignment.AttemptID, now)
			}
			if discardErr != nil && supervisor(attemptCtx).wasRequested() {
				discarded, discardErr = launch.DiscardCancelled(a.Identity, assignment.AttemptID)
			}
			if discardErr == nil {
				if e = send("command-events", discarded); e != nil {
					return false, e
				}
				result.Commands = append(result.Commands, runs.CommandResult{Ordinal: request.Ordinal, Permission: permission.ID, Status: "not_executed", Stop: discarded})
				return finish("not_executed")
			}
		}
		if event.Event != "stopped" || event.GroupEmpty == nil || !*event.GroupEmpty || errors.Is(runErr, source.ErrProcess) {
			// Explicit uncertainty opens formal recovery and retains ownership.
			result.Outcome = "unknown"
			result.EverPermitted = true
			_ = send("result", result)
			return false, source.ErrProcess
		}
		if e = send("command-events", event); e != nil {
			return false, e
		}
		result.Commands = append(result.Commands, runs.CommandResult{Ordinal: request.Ordinal, Permission: permission.ID, Status: "stopped", Exit: event.Exit, Signal: event.Signal, Stop: event})
		if runErr != nil || event.Exit == nil || (*event.Exit != 0 && (command != "plan" || *event.Exit != 2)) {
			return finish("failure")
		}
		if command == "init" {
			if e = verifyTrees(reviewed, trees, lockPath, s.Source.RepositoryID); e != nil {
				return finish("failure")
			}
			manifest = runs.Manifest{ReviewedInputs: reviewed, Providers: []runs.Provider{}}
			lock, e := protectedBytes(filepath.Join(tree, lockPath), 67108864)
			lockID := ""
			if e == nil {
				manifest.Lock = runs.ProviderLock{Present: true, Hash: hash(lock), Size: int64(len(lock))}
				manifest.Providers, e = runs.ParseProviderLock(lock)
				if e != nil {
					return finish("failure")
				}
				if pre.LockOrigin == "tracked" && manifest.Lock.Hash != pre.LockHash {
					return finish("failure")
				}
				if e = manifest.ValidateLock(); e != nil {
					return finish("failure")
				}
				lockID, e = retain("provider_lock", lock)
				if e != nil {
					return finish("failure")
				}
			} else if !os.IsNotExist(fileError(filepath.Join(tree, lockPath))) {
				return finish("failure")
			}
			if e = verifyInstalledModules(dir, filepath.Join(tree, s.Source.Root), trees, inspected); e != nil {
				return finish("failure")
			}
			manifestID, e := retain("manifest", manifest)
			if e != nil {
				return finish("failure")
			}
			result.ManifestDigest = runs.Digest(manifest)
			if e = send("manifest-seal", runs.ManifestSeal{PreInitDigest: result.PreInitDigest, Artifact: manifestID, Digest: result.ManifestDigest, LockArtifact: lockID, Providers: manifest.Providers}); e != nil {
				var rejected *Rejected
				if errors.As(e, &rejected) && rejected.Status >= 400 && rejected.Status < 500 {
					// A rejected seal issues no launch authority. Init is positively stopped;
					// finish with the accepted pre-init identity, without claiming a seal.
					result.ManifestDigest = ""
					return finish("failure")
				}
				return false, e
			}
		}
		if command == "schema" {
			schema, e = protectedBytes(filepath.Join(dir, "schema.json"), plan.MaxBytes)
			if e != nil {
				return finish("failure")
			}
			if e = (plan.Verifier{}).VerifySchema(ctx, s, manifest, schema); e != nil {
				return finish("failure")
			}
			if _, e = retain("provider_schema", schema); e != nil {
				return finish("failure")
			}
		}
		if command == "plan" {
			binary, e := protectedBytes(filepath.Join(dir, "plan.bin"), plan.MaxBytes)
			if e != nil || len(binary) == 0 {
				return finish("failure")
			}
			savedHash = hash(binary)
			if _, e = retain("binary_plan", binary); e != nil {
				return finish("failure")
			}
		}
		if command == "show" {
			raw, e = protectedBytes(filepath.Join(dir, "show.json"), plan.MaxBytes)
			if e != nil {
				return finish("failure")
			}
			if _, e = retain("raw_plan_json", raw); e != nil {
				return finish("failure")
			}
		}
	}
	if e = verifyTrees(reviewed, trees, lockPath, s.Source.RepositoryID); e != nil {
		return finish("failure")
	}
	if saved, e := protectedBytes(filepath.Join(dir, "plan.bin"), plan.MaxBytes); e != nil || hash(saved) != savedHash {
		return finish("failure")
	}
	if e = verifyNativeLock(filepath.Join(tree, lockPath), manifest); e != nil {
		return finish("failure")
	}
	options, ids, e := plan.Allocate(raw, plan.Options{Engine: s.Engine.Name, Version: s.Engine.Version, ProfileID: s.ProfileID, Providers: manifest.Providers, Secrets: reg}, nil, runs.ID)
	if e != nil {
		return finish("failure")
	}
	analysis, e := plan.Analyze(raw, schema, options)
	if e != nil {
		return finish("failure")
	}
	safe, e := canonicalArtifact(plan.Analysis{Identities: ids, Changes: analysis})
	if e != nil || plan.ValidateManagedSecrets(safe, secrets) != nil {
		return finish("failure")
	}
	findings, e := canonicalArtifact(analysis.Findings)
	if e != nil || plan.ValidateManagedSecrets(findings, secrets) != nil {
		return finish("failure")
	}
	if _, e = retain("normalized_changes", safe); e != nil {
		return finish("failure")
	}
	if _, e = retain("findings", findings); e != nil {
		return finish("failure")
	}
	return finish("success")
}
func fileError(p string) error { _, e := os.Lstat(p); return e }

func verifyNativeLock(p string, m runs.Manifest) error {
	if !m.Lock.Present {
		if !os.IsNotExist(fileError(p)) {
			return ErrAgent
		}
		return nil
	}
	raw, e := protectedBytes(p, 67108864)
	if e != nil {
		return e
	}
	return runs.VerifyProviderLock(&m, raw)
}
