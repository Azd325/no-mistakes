package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"gopkg.in/yaml.v3"
)

// retryLaunch is what a continuation run takes from the failed run it
// continues: the run itself, whose record the executor re-proves and carries,
// and the audit link written before the continuation can execute anything.
type retryLaunch struct {
	source *db.Run
	link   db.RunRetry
}

// record writes the audit link and the failed run's exact intent onto the
// continuation run. The intent is copied byte for byte with its provenance
// because a carried Intent step never runs again to produce it.
func (r *retryLaunch) record(database *db.DB, run *db.Run) error {
	link := r.link
	link.RunID = run.ID
	if err := database.InsertRunRetry(link); err != nil {
		return err
	}
	if r.source.Intent == nil {
		return nil
	}
	intent := db.RunIntent{Summary: *r.source.Intent}
	if r.source.IntentSource != nil {
		intent.Source = *r.source.IntentSource
	}
	if r.source.IntentSessionID != nil {
		intent.SessionID = *r.source.IntentSessionID
	}
	if r.source.IntentScore != nil {
		intent.Score = *r.source.IntentScore
	}
	if err := database.UpdateRunIntent(run.ID, intent); err != nil {
		return fmt.Errorf("copy the failed run's intent: %w", err)
	}
	run.Intent, run.IntentSource, run.IntentSessionID, run.IntentScore = &intent.Summary, &intent.Source, &intent.SessionID, &intent.Score
	return nil
}

// retryRefusal names the proof a retry request failed. Every refusal leaves
// the failed run, the branch, and the gate untouched.
type retryRefusal struct {
	runID  string
	proof  string
	detail string
}

func (r *retryRefusal) Error() string {
	return fmt.Sprintf("refusing retry of run %s: %s proof failed: %s; `no-mistakes rerun` validates the branch from the start instead", r.runID, r.proof, r.detail)
}

func refuseRetry(runID, proof, format string, args ...any) error {
	return &retryRefusal{runID: runID, proof: proof, detail: fmt.Sprintf(format, args...)}
}

// HandleRetry continues runID, the branch's latest run, in a new run that
// starts at the failed run's preserved head: the steps before its failed step
// are carried, the failed step restarts from the beginning, and every later
// step runs normally. It refuses, changing nothing, unless every proof holds:
// the run is this branch's latest and failed while nothing is active; its
// record names exactly one failed step, failed on an agent invocation error,
// with nothing from Test on carried and nothing published; its preserved head
// is anchored; the caller is clean at exactly that head; and the model profile
// and the repository's gates are unchanged.
func (m *RunManager) HandleRetry(ctx context.Context, repoID, branch, runID, callerHeadSHA string) (*db.RunRetry, error) {
	repo, err := m.db.GetRepo(repoID)
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return nil, fmt.Errorf("unknown repo %s", repoID)
	}
	var link db.RunRetry
	_, err = m.withBranchLock(repo.ID, branch, func() (string, error) {
		launch, err := m.proveRetry(ctx, repo, branch, runID, callerHeadSHA)
		if err != nil {
			return "", err
		}
		source := launch.source
		prBaseBranch := ""
		if source.PRBaseBranch != nil {
			prBaseBranch = strings.TrimSpace(*source.PRBaseBranch)
		}
		var profiles []*agentcfg.PiProfile
		if source.PiProfile != nil {
			profiles = append(profiles, source.PiProfile)
		}
		continuation, err := m.startRunWithIntentSourceLocked(ctx, repo, branch, source.HeadSHA, source.BaseSHA, "retry", nil, "", "", "", "", "", prBaseBranch, source.OmitIntent, inheritablePRURL(source), "", source.ClosingIssueRefs, launch, profiles...)
		link = launch.link
		link.RunID = continuation
		return continuation, err
	})
	if err != nil {
		return nil, err
	}
	return &link, nil
}

func (m *RunManager) proveRetry(ctx context.Context, repo *db.Repo, branch, runID, callerHeadSHA string) (*retryLaunch, error) {
	source, err := m.db.GetRun(runID)
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	if source == nil || source.RepoID != repo.ID || source.Branch != branch {
		return nil, refuseRetry(runID, "run identity", "the run does not belong to branch %s of this repository", branch)
	}
	runs, err := m.db.GetRunsByRepo(repo.ID)
	if err != nil {
		return nil, fmt.Errorf("get runs: %w", err)
	}
	for _, run := range runs {
		if run.Branch != branch {
			continue
		}
		if run.ID != source.ID {
			return nil, refuseRetry(runID, "latest run", "run %s is newer on branch %s, so this run's head may no longer be the branch's", run.ID, branch)
		}
		break
	}
	active, err := m.db.GetActiveRun(repo.ID, branch)
	if err != nil {
		return nil, fmt.Errorf("get active run: %w", err)
	}
	if active != nil {
		return nil, refuseRetry(runID, "no active run", "run %s is active on branch %s", active.ID, branch)
	}
	if source.Status != types.RunFailed {
		return nil, refuseRetry(runID, "failed run", "the run is %s, and only a failed run can continue", source.Status)
	}
	if source.LastPushedSHA != nil || source.PushGeneration != nil {
		return nil, refuseRetry(runID, "unpublished run", "the run already pushed, so its head is no longer only in the local gate")
	}
	if source.VerificationPlan != nil {
		return nil, refuseRetry(runID, "compatible configuration", "the run carries a verification plan, which belongs to that run alone")
	}
	gates, err := m.pinnedRunGates(source.ID)
	if err != nil {
		return nil, refuseRetry(runID, pipeline.RetryProofCheckpoint, "%v", err)
	}
	checkpoint, err := pipeline.PlanRetryCheckpoint(m.db, source, steps.WithCustomGates(m.steps(), gates))
	if err != nil {
		var refusal *pipeline.RetryRefusal
		if errors.As(err, &refusal) {
			return nil, refuseRetry(runID, refusal.Proof, "%s", refusal.Reason)
		}
		return nil, err
	}
	if err := m.provePreservedHead(ctx, repo, source); err != nil {
		return nil, refuseRetry(runID, "preserved head", "%v", err)
	}
	switch callerHeadSHA {
	case "":
		return nil, refuseRetry(runID, "clean branch", "the working tree has uncommitted changes, so its head is not a proof")
	case source.HeadSHA:
	default:
		return nil, refuseRetry(runID, "matching head", "the clean local head %s is not the run's preserved head %s; inspect `no-mistakes axi status` and follow `branch_sync.next_action.command` (`no-mistakes axi sync --recover` adopts the preserved head) before retrying", callerHeadSHA, source.HeadSHA)
	}
	if err := m.proveRetryConfiguration(ctx, repo, source, gates, checkpoint); err != nil {
		return nil, refuseRetry(runID, "compatible configuration", "%v", err)
	}
	failed := checkpoint.Failed()
	sourceError := ""
	if failed.Error != nil {
		sourceError = *failed.Error
	}
	return &retryLaunch{source: source, link: db.RunRetry{
		SourceRunID:       source.ID,
		StepName:          failed.StepName,
		SourceRound:       checkpoint.Round,
		SourceError:       sourceError,
		CheckpointHeadSHA: source.HeadSHA,
	}}, nil
}

// provePreservedHead requires the head the failed run verified when it ended
// to still be the exact commit the gate keeps for it: its recovery anchor when
// the pipeline moved the head, the submitted commit otherwise.
func (m *RunManager) provePreservedHead(ctx context.Context, repo *db.Repo, source *db.Run) error {
	if source.TerminalHeadVerifiedAt == nil {
		return fmt.Errorf("the run's final head was not verified when it failed")
	}
	gateDir := m.paths.RepoDir(repo.ID)
	if source.SubmittedHeadSHA != nil && *source.SubmittedHeadSHA == source.HeadSHA {
		if preserved, err := git.Run(ctx, gateDir, "rev-parse", source.HeadSHA+"^{commit}"); err != nil || preserved != source.HeadSHA {
			return fmt.Errorf("head %s is not available in the local gate", source.HeadSHA)
		}
		return nil
	}
	ref := custody.RecoveryRef(source.ID)
	_, exists, err := git.ExactRefTarget(ctx, gateDir, ref)
	if err != nil {
		return fmt.Errorf("inspect recovery anchor %s: %w", ref, err)
	}
	if !exists {
		return fmt.Errorf("recovery anchor %s is missing", ref)
	}
	preserved, err := git.Run(ctx, gateDir, "rev-parse", ref+"^{commit}")
	if err != nil {
		return fmt.Errorf("recovery anchor %s does not name a commit", ref)
	}
	if preserved != source.HeadSHA {
		return fmt.Errorf("recovery anchor %s points at %s, not the run's head %s", ref, preserved, source.HeadSHA)
	}
	return nil
}

// proveRetryConfiguration requires what the failed run pinned at creation to
// resolve the same way now: the model profile, and the gates, which decide the
// step sequence the carried record is matched against. It also requires the
// configuration that produced every carried step result to be the one a new
// run would use: the repository configuration the carried Review rounds
// recorded, and the agent that served the carried steps.
func (m *RunManager) proveRetryConfiguration(ctx context.Context, repo *db.Repo, source *db.Run, pinned []config.Gate, checkpoint *pipeline.RetryCheckpoint) error {
	globalCfg, err := config.LoadGlobal(m.paths.ConfigFile())
	if err != nil {
		return fmt.Errorf("load global config: %w", err)
	}
	if source.PiProfile != nil {
		resolved, err := globalCfg.ResolvePiProfile(source.PiProfile)
		if err != nil {
			return fmt.Errorf("the run's model profile no longer resolves: %w", err)
		}
		if *resolved != *source.PiProfile {
			return fmt.Errorf("the run's model profile now resolves to %s/%s instead of %s/%s", resolved.Model, resolved.Effort, source.PiProfile.Model, source.PiProfile.Effort)
		}
	}
	cfg, currentRepoCfg, err := m.effectiveConfigBeforeRun(ctx, repo, source.HeadSHA, globalCfg, "retry configuration")
	if err != nil {
		return err
	}
	current, err := config.MarshalGates(cfg.Gates)
	if err != nil {
		return fmt.Errorf("resolve the repository's gates: %w", err)
	}
	recorded, err := config.MarshalGates(pinned)
	if err != nil {
		return fmt.Errorf("read the run's gates: %w", err)
	}
	if current != recorded {
		return fmt.Errorf("the repository's gates changed since the run started, so its step sequence would differ")
	}
	carried := checkpoint.Steps[:checkpoint.Index]
	if err := m.proveCarriedRepoConfig(ctx, repo, source, currentRepoCfg, carried); err != nil {
		return err
	}
	return m.proveCarriedAgent(source, cfg, carried)
}

// proveCarriedRepoConfig compares the repository configuration a carried
// Review round ran under, rebuilt from the trusted default-branch commit that
// round recorded, with the configuration a new run would get now.
func (m *RunManager) proveCarriedRepoConfig(ctx context.Context, repo *db.Repo, source *db.Run, current *config.RepoConfig, carried []*db.StepResult) error {
	for _, row := range carried {
		if row.StepName != types.StepReview || row.Status != types.StepStatusCompleted {
			continue
		}
		rounds, err := m.db.GetRoundsByStep(row.RoundsStepID())
		if err != nil {
			return fmt.Errorf("read the carried review rounds: %w", err)
		}
		recordedSHA := ""
		for _, round := range rounds {
			if round.TrustedConfigSHA != nil {
				recordedSHA = *round.TrustedConfigSHA
			}
		}
		if recordedSHA == "" {
			return fmt.Errorf("the carried review recorded no trusted configuration to compare, so rerun the pipeline instead")
		}
		recorded, err := effectiveRepoConfigAt(ctx, m.paths.RepoDir(repo.ID), repo, recordedSHA, source.HeadSHA, "retry configuration")
		if err != nil {
			return err
		}
		recordedYAML, err := yaml.Marshal(recorded)
		if err != nil {
			return fmt.Errorf("serialize the carried review's repository configuration: %w", err)
		}
		currentYAML, err := yaml.Marshal(current)
		if err != nil {
			return fmt.Errorf("serialize the repository configuration: %w", err)
		}
		if !bytes.Equal(recordedYAML, currentYAML) {
			return fmt.Errorf("the repository configuration changed since the carried review ran under trusted commit %s, so its approval does not carry; rerun the pipeline instead", recordedSHA)
		}
	}
	return nil
}

// proveCarriedAgent requires every agent that served a carried step to still be
// selected. A run pinned to a Pi profile is proven by the profile instead.
func (m *RunManager) proveCarriedAgent(source *db.Run, cfg *config.Config, carried []*db.StepResult) error {
	if source.PiProfile != nil || cfg.Agent == types.AgentAuto || slices.Contains(cfg.Agents, types.AgentAuto) {
		return nil
	}
	carriedSteps := make(map[string]bool, len(carried))
	for _, row := range carried {
		carriedSteps[string(row.StepName)] = true
	}
	invocations, err := m.db.GetAgentInvocationsByRun(source.ID)
	if err != nil {
		return fmt.Errorf("read the run's agent invocations: %w", err)
	}
	for _, inv := range invocations {
		if !carriedSteps[inv.StepName] || inv.ExitStatus != "ok" {
			continue
		}
		name := types.AgentName(inv.Agent)
		if name != cfg.Agent && !slices.Contains(cfg.Agents, name) {
			return fmt.Errorf("carried step %s ran on agent %s, which is no longer selected (agent is now %s); rerun the pipeline instead", inv.StepName, inv.Agent, cfg.Agent)
		}
	}
	return nil
}
