//go:build e2e

package e2e

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func prepareHistoryIntegration(t *testing.T, h *Harness, branch string) (string, string, string) {
	t.Helper()
	initFromOwnWorktree(t, h, "init-history")
	preserved := h.CommitChange("preserved", "feature.txt", "preserved work\n", "preserved feature")
	base := advanceMain(t, h, "base.txt", "main advanced\n", "advance main")
	gitIn(t, h, h.WorkDir, "checkout", "-b", branch, base)
	gitIn(t, h, h.WorkDir, "merge", "--no-ff", preserved, "-m", "explicit integration")
	head := gitIn(t, h, h.WorkDir, "rev-parse", "HEAD")
	h.Checkout("main")
	return h.AddWorktree(branch), head, base
}

func TestPreserveHistoryPublishesExactTwoParentIntegrationJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	const branch = "feature/pinned-integration"
	operator, head, base := prepareHistoryIntegration(t, h, branch)
	parents := parentsOf(t, h, operator, head)
	if len(parents) != 2 || parents[0] != base {
		t.Fatalf("fixture is not the intended two-parent integration: %v", parents)
	}
	gitIn(t, h, h.UpstreamDir, "config", "receive.denyNonFastForwards", "true")
	out, err := h.RunInDir(operator, "axi", "run", "--intent", "preserve both exact integration histories", "--base-branch", "main", "--preserve-history", base)
	if err != nil {
		t.Fatalf("constrained CLI launch: %v\n%s", err, out)
	}
	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted || run.SubmittedHeadSHA == nil || *run.SubmittedHeadSHA != head ||
		run.PreserveHistoryBaseSHA == nil || *run.PreserveHistoryBaseSHA != base {
		t.Fatalf("completed constrained run: %+v\n%s", run, out)
	}
	for _, name := range []types.StepName{types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush} {
		step, ok := findStep(run.Steps, name)
		if !ok || step.Status != types.StepStatusCompleted {
			t.Fatalf("required stage %s was not completed: %+v", name, step)
		}
	}
	// The harness upstream is a bare repo with no forge, so delivery skips as
	// it does for every other journey; publication is still the real thing.
	if errs := validateSkippedSteps(run.Steps, types.StepPR, types.StepCI); len(errs) != 0 {
		t.Fatal(strings.Join(errs, "; "))
	}
	published := h.UpstreamBranchSHA(branch)
	if !isAncestorIn(t, h, h.UpstreamDir, head, published) || !reflect.DeepEqual(parentsOf(t, h, h.UpstreamDir, head), parents) {
		t.Fatal("publication lost the exact submitted integration or its parent order")
	}
	if gitIn(t, h, operator, "rev-parse", "HEAD") != head {
		t.Fatal("validation rewrote the invoking worktree")
	}
}

func TestPreserveHistoryMovedBaseWhileParkedStopsWithoutPublicationJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t)})
	const branch = "feature/pinned-base-moved"
	operator, head, base := prepareHistoryIntegration(t, h, branch)
	out, err := h.RunInDir(operator, "axi", "run", "--intent", "preserve both exact integration histories", "--base-branch", "main", "--preserve-history", base)
	if err != nil || !strings.Contains(out, "gate:") {
		t.Fatalf("expected review gate: %v\n%s", err, out)
	}
	run := h.ActiveRun(branch)
	if run == nil {
		t.Fatal("missing parked run")
	}
	advanceMain(t, h, "later.txt", "later base\n", "advance pinned base while parked")
	// This is the isolated fake-review gate, not an operator's real finding.
	h.Respond(run.ID, types.StepReview, types.ActionFix)
	failed := h.WaitForRun(branch, 90*time.Second)
	if failed.Status != types.RunFailed || failed.Error == nil || !strings.Contains(*failed.Error, "integration base main moved") {
		t.Fatalf("moved pinned base was not refused: %+v", failed)
	}
	if gitIn(t, h, operator, "rev-parse", "HEAD") != head || failed.HeadSHA != head {
		t.Fatal("refusal changed the original or recorded integration head")
	}
	if _, err := h.runGit(context.Background(), h.UpstreamDir, "show-ref", "--verify", "refs/heads/"+branch); err == nil {
		t.Fatal("a base-constrained refusal was published")
	}
}
