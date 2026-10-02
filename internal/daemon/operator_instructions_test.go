package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func pathInstructionsYAML(indent, prefix string, n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "%s- path: '%s%d/**'\n%s  instructions: rule %d\n", indent, prefix, i, indent, i)
	}
	return b.String()
}

// Each config file fits the review-prompt caps on its own, but the operator's
// global rules plus the repository's trusted rules do not; the run must fail
// before any step runs instead of handing the reviewer an oversized prompt.
func TestStartRun_FailsClosedWhenOperatorAndTrustedPathInstructionsOverrunTheCap(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{step} })

	global := "review:\n  path_instructions:\n" + pathInstructionsYAML("    ", "g", 20)
	if err := os.WriteFile(p.ConfigFile(), []byte(global), 0o644); err != nil {
		t.Fatal(err)
	}

	repo, _ := setupTestGitRepo(t, p, d, "operator-budget-repo")
	trusted := "review:\n  path_instructions:\n" + pathInstructionsYAML("    ", "t", 20)
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, ".no-mistakes.yaml"), []byte(trusted), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "commit", "-am", "trusted review rules")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
	headSHA := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("operator-budget-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result)
	if err == nil {
		run := waitForRunTerminalState(t, d, result.RunID)
		var runErr string
		if run.Error != nil {
			runErr = *run.Error
		}
		err = fmt.Errorf("status %s: %s", run.Status, runErr)
		if run.Status == types.RunCompleted {
			t.Fatalf("run completed with 40 combined path instructions, want it to fail closed (%v)", err)
		}
	}
	if !strings.Contains(err.Error(), "40 entries combined (20 machine-local global, 20 trusted)") {
		t.Fatalf("run start error = %v, want the combined path-instruction cap named", err)
	}
	if step.execCnt.Load() != 0 {
		t.Fatalf("a step ran %d time(s) after the combined cap was exceeded", step.execCnt.Load())
	}
}
