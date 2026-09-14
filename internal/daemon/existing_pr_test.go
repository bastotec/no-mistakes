package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExistingPRLaunchPinsTargetAndFailsClosed(t *testing.T) {
	bin := t.TempDir()
	name := "gh"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(bin, name), "../pipeline/fakecli")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake gh: %v %s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "gh")
	const target = "https://github.com/upstream/widgets/pull/168"
	var calls atomic.Int32
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&existingPRObserveStep{calls: &calls, target: target}} })
	repo, _ := setupTestGitRepo(t, p, d, "explicit-pr")
	var err error
	repo, err = d.UpdateRepoForkURL(repo.ID, "https://github.com/contributor/widgets.git")
	if err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "new.txt"), []byte("new committed work"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "new work")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	payload := fmt.Sprintf(`{"number":168,"html_url":%q,"state":"open","base":{"ref":"main","repo":{"full_name":"upstream/widgets","html_url":"https://github.com/upstream/widgets"}},"head":{"ref":"feature","sha":%q,"repo":{"full_name":"contributor/widgets","html_url":"https://github.com/contributor/widgets"}}}`, target, head)
	t.Setenv("FAKE_CLI_EXISTING_PR_JSON", payload)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := ipc.StartExistingPRRunParams{RepoID: repo.ID, Branch: "feature", HeadSHA: head, Intent: "validate existing upstream PR", URL: target}
	var result ipc.RerunResult
	if err := client.Call(ipc.MethodStartExistingPRRun, &request, &result); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, d, result.RunID)
	if run.Status != types.RunCompleted || run.ExistingPRURL == nil || *run.ExistingPRURL != target {
		t.Fatalf("run lost target: %+v", run)
	}
	if calls.Load() != 1 {
		t.Fatalf("step calls=%d", calls.Load())
	}
	if got := inheritablePRURL(run); got != "" {
		t.Fatalf("explicit URL leaked into legacy host discovery: %s", got)
	}
	legacy := *run
	legacy.ExistingPRURL = nil
	if got := inheritablePRURL(&legacy); got != target {
		t.Fatalf("legacy inheritance changed: %s", got)
	}
	// Transfer was object-only, not an ordinary push triggering another run.
	if _, err := exec.Command("git", "-C", p.RepoDir(repo.ID), "show-ref", "--verify", "refs/heads/feature").Output(); err == nil {
		t.Fatal("launch unexpectedly replaced gate branch")
	}

	// A rerun inherits the hard constraint, not just the legacy discovered URL.
	gitCmd(t, p.RepoDir(repo.ID), "update-ref", "refs/heads/feature", head)
	var rerun ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "feature", PreviousRunID: run.ID}, &rerun); err != nil {
		t.Fatal(err)
	}
	inherited := waitForRunTerminalState(t, d, rerun.RunID)
	if inherited.Status != types.RunCompleted || inherited.ExistingPRURL == nil || *inherited.ExistingPRURL != target {
		t.Fatalf("rerun lost target: %+v", inherited)
	}
	before := calls.Load()
	for _, tc := range []struct{ name, payload, head string }{
		{"wrong live head", strings.Replace(payload, head, strings.Repeat("f", 40), 1), head},
		{"wrong source", strings.Replace(payload, `"ref":"feature"`, `"ref":"other"`, 1), head},
		{"unreadable PR", "{}", head},
		{"wrong submitted head", payload, strings.Repeat("a", 40)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FAKE_CLI_EXISTING_PR_JSON", tc.payload)
			req := request
			req.HeadSHA = tc.head
			if err := client.Call(ipc.MethodStartExistingPRRun, &req, &ipc.RerunResult{}); err == nil {
				t.Fatal("invalid explicit association started")
			}
			if calls.Load() != before {
				t.Fatal("invalid explicit association ran pipeline")
			}
		})
	}
	active, err := d.InsertRun(repo.ID, "feature", head, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Call(ipc.MethodStartExistingPRRun, &request, &ipc.RerunResult{}); err == nil {
		t.Fatal("explicit launch replaced an unbound active run")
	}
	stillActive, err := d.GetRun(active.ID)
	if err != nil || stillActive.Status != types.RunPending {
		t.Fatalf("refused launch cancelled active run: %+v %v", stillActive, err)
	}
}

// The integration branch of an explicit run is the branch its pull request
// actually targets. Reading the repository default instead rebases, pushes and
// reports against another branch than the one CI merges into.
func TestExistingPRLaunchIntegratesWithTheValidatedBaseBranch(t *testing.T) {
	bin := t.TempDir()
	name := "gh"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(bin, name), "../pipeline/fakecli")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake gh: %v %s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "gh")
	const target = "https://github.com/upstream/widgets/pull/168"
	const prBase = "release/2.0"
	var calls atomic.Int32
	observed := &existingPRObserveStep{calls: &calls, target: target, base: prBase}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{observed} })
	repo, _ := setupTestGitRepo(t, p, d, "explicit-pr-base")
	repo, err := d.UpdateRepoForkURL(repo.ID, "https://github.com/contributor/widgets.git")
	if err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "new.txt"), []byte("new committed work"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "new work")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	payload := fmt.Sprintf(`{"number":168,"html_url":%q,"state":"open","base":{"ref":%q,"repo":{"full_name":"upstream/widgets","html_url":"https://github.com/upstream/widgets"}},"head":{"ref":"feature","sha":%q,"repo":{"full_name":"contributor/widgets","html_url":"https://github.com/contributor/widgets"}}}`, target, prBase, head)
	t.Setenv("FAKE_CLI_EXISTING_PR_JSON", payload)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.RerunResult
	if err := client.Call(ipc.MethodStartExistingPRRun, &ipc.StartExistingPRRunParams{RepoID: repo.ID, Branch: "feature", HeadSHA: head, Intent: "validate existing upstream PR", URL: target}, &result); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, d, result.RunID)
	if run.Status != types.RunCompleted || run.PRBaseBranch == nil || *run.PRBaseBranch != prBase {
		t.Fatalf("run did not adopt the pull request base: %+v", run)
	}

	// A rerun reads the base back from the live pull request rather than
	// inheriting it as an operator override, which explicit runs refuse.
	gitCmd(t, p.RepoDir(repo.ID), "update-ref", "refs/heads/feature", head)
	var rerun ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "feature", PreviousRunID: run.ID}, &rerun); err != nil {
		t.Fatal(err)
	}
	inherited := waitForRunTerminalState(t, d, rerun.RunID)
	if inherited.Status != types.RunCompleted || inherited.PRBaseBranch == nil || *inherited.PRBaseBranch != prBase {
		t.Fatalf("rerun lost the pull request base: %+v", inherited)
	}
	if calls.Load() != 2 {
		t.Fatalf("step calls=%d", calls.Load())
	}
}

type existingPRObserveStep struct {
	calls  *atomic.Int32
	target string
	base   string
}

func (s *existingPRObserveStep) Name() types.StepName { return types.StepReview }
func (s *existingPRObserveStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.calls.Add(1)
	if ctx.Run.ExistingPRURL == nil || *ctx.Run.ExistingPRURL != s.target || ctx.Run.PRURL == nil || *ctx.Run.PRURL != s.target {
		return nil, fmt.Errorf("target missing before first step")
	}
	if s.base != "" && (ctx.Run.PRBaseBranch == nil || *ctx.Run.PRBaseBranch != s.base) {
		return nil, fmt.Errorf("step ran against %v, not the pull request base %s", ctx.Run.PRBaseBranch, s.base)
	}
	return &pipeline.StepOutcome{}, nil
}
