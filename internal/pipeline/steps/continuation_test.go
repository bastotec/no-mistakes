package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type continuationFixture struct {
	s                                                    *pipeline.StepContext
	base, published, submitted, tip, fork, upstream, log string
}

// Both GitHub URLs are rewritten to disposable bare repos. The forge is the
// tiny fake CLI: these fixtures perform no provider, credential or product IO.
func newContinuationFixture(t *testing.T) *continuationFixture {
	t.Helper()
	dir, base, published := setupGitRepo(t)
	fork, upstream := t.TempDir(), t.TempDir()
	gitCmd(t, fork, "init", "--bare")
	gitCmd(t, upstream, "init", "--bare")
	gitCmd(t, dir, "remote", "add", "origin", fork)
	gitCmd(t, dir, "push", fork, "main", published+":refs/heads/feature")
	gitCmd(t, dir, "checkout", "main")
	writeFixtureFile(t, dir, "advanced.txt", "advanced main\n")
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-m", "advance main")
	tip := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", upstream, "main")
	gitCmd(t, dir, "checkout", "feature")
	writeFixtureFile(t, dir, "correction.txt", "unpublished correction\n")
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-m", "unpublished correction")
	submitted := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "config", "url."+upstream+".insteadOf", "https://github.com/upstream/widgets.git")
	gitCmd(t, dir, "config", "url."+fork+".insteadOf", fixtureSourceURL)
	s := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, base, submitted, config.Commands{})
	pinFixturePRWithBase(t, s, "main")
	pins := db.ContinuationPins{Version: 1, PRHead: published, BaseTip: tip, MergeBase: base, BaseBranch: "main", URL: fixtureExistingPR}
	raw, err := json.Marshal(pins)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.AssociateRunWithExistingPR(s.Run.ID, s.Repo.ID, "feature", fixtureExistingPR, "main", string(raw)); err != nil {
		t.Fatal(err)
	}
	s.Run, err = s.DB.GetRun(s.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	env, log := fakeGH(t, "")
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(bodyFile, []byte("Author narrative.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	env = append(env, "FAKE_CLI_PR_BODY_FILE="+bodyFile)
	s.Env = append(env, "FAKE_CLI_EXISTING_PR_JSON="+existingPRFixture(published), "FAKE_CLI_EXISTING_PR_ENDPOINT=repos/upstream/widgets/pulls/168")
	if err := FetchRunUpstreamBranch(s.Ctx, s, "main"); err != nil {
		t.Fatal(err)
	}
	return &continuationFixture{s: s, base: base, published: published, submitted: submitted, tip: tip, fork: fork, upstream: upstream, log: log}
}

func (f *continuationFixture) setPR(head, base string) {
	for i, e := range f.s.Env {
		if strings.HasPrefix(e, "FAKE_CLI_EXISTING_PR_JSON=") {
			f.s.Env[i] = "FAKE_CLI_EXISTING_PR_JSON=" + strings.Replace(existingPRFixture(head), `"ref":"main"`, `"ref":"`+base+`"`, 1)
		}
	}
}

func TestContinuation_AdvancedMainRemainsUnintegratedAndTestExcluded(t *testing.T) {
	f := newContinuationFixture(t)
	if err := PrepareContinuation(f.s, f.published); err != nil {
		t.Fatal(err)
	}
	if _, err := (&RebaseStep{}).Execute(f.s); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, f.s.WorkDir, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatal("integration moved submitted head")
	}
	base, err := resolveBranchBaseSHA(f.s.Ctx, f.s, "", "main")
	if err != nil || base != f.base {
		t.Fatalf("base=%s err=%v", base, err)
	}
	f.s.Config.Commands.Test = "exit 93"
	f.s.Agent = &mockAgent{name: "forbidden", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("local Test agent launched")
		return nil, nil
	}}
	out, err := (&TestStep{}).Execute(f.s)
	if err != nil || !out.Skipped || !strings.Contains(out.SkipReason, "local Test explicitly excluded") {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestContinuation_AdmissionRejectsEqualSiblingAndBackwardHeads(t *testing.T) {
	for _, which := range []string{"equal", "sibling", "backward"} {
		t.Run(which, func(t *testing.T) {
			f := newContinuationFixture(t)
			switch which {
			case "equal":
				f.s.Run.HeadSHA = f.published
			case "backward":
				f.s.Run.HeadSHA = f.base
			case "sibling":
				gitCmd(t, f.s.WorkDir, "checkout", "--detach", f.base)
				gitCmd(t, f.s.WorkDir, "commit", "--allow-empty", "-m", "sibling")
				f.s.Run.HeadSHA = gitCmd(t, f.s.WorkDir, "rev-parse", "HEAD")
			}
			if err := PrepareContinuation(f.s, f.published); err == nil {
				t.Fatal("invalid admission accepted")
			}
			if got := gitCmd(t, f.fork, "rev-parse", "feature"); got != f.published {
				t.Fatal("refused admission published")
			}
		})
	}
}

func TestContinuation_ExternalMovementRefusesBeforeAnyAgentOrPublication(t *testing.T) {
	for _, which := range []string{"base-tip", "pr-base", "pr-head", "source-head", "deleted-source"} {
		t.Run(which, func(t *testing.T) {
			f := newContinuationFixture(t)
			switch which {
			case "base-tip":
				gitCmd(t, f.upstream, "update-ref", "refs/heads/main", f.base)
			case "pr-base":
				f.setPR(f.published, "other")
			case "pr-head":
				f.setPR(f.submitted, "main")
			case "source-head":
				gitCmd(t, f.s.WorkDir, "push", f.fork, f.submitted+":refs/heads/feature")
			case "deleted-source":
				gitCmd(t, f.fork, "update-ref", "-d", "refs/heads/feature")
			}
			calls := 0
			f.s.Agent = &mockAgent{name: "forbidden", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) { calls++; return nil, nil }}
			f.s.CheckRunPolicy = func() error { return AssertContinuationPolicy(f.s) }
			if _, err := f.s.RunAgent(agent.RunOpts{CWD: f.s.WorkDir}); err == nil || calls != 0 {
				t.Fatalf("agent crossed refusal: calls=%d err=%v", calls, err)
			}
			if err := publishRunHead(f.s, f.submitted, "", nil); err == nil {
				t.Fatal("publication crossed refusal")
			}
			log, _ := os.ReadFile(f.log)
			if strings.Contains(string(log), "pr edit") || strings.Contains(string(log), "pr create") {
				t.Fatalf("refusal mutated forge: %s", log)
			}
		})
	}
}

func TestContinuation_RepairRewriteAndIntegrationAreRejected(t *testing.T) {
	for _, which := range []string{"rewrite", "merge-base"} {
		t.Run(which, func(t *testing.T) {
			f := newContinuationFixture(t)
			f.s.Agent = &mockAgent{name: "rewrite", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if which == "rewrite" {
					gitCmd(t, opts.CWD, "reset", "--hard", f.published)
					gitCmd(t, opts.CWD, "commit", "--allow-empty", "-m", "rewritten correction")
				} else {
					gitCmd(t, opts.CWD, "merge", "--no-ff", f.tip, "-m", "unauthorized integration")
				}
				return &agent.Result{Output: []byte(`{"summary":"repair"}`)}, nil
			}}
			f.s.CheckRunPolicy = func() error { return AssertContinuationPolicy(f.s) }
			if _, err := f.s.RunAgent(agent.RunOpts{CWD: f.s.WorkDir}); err == nil {
				t.Fatal("incompatible repair accepted")
			}
			if _, err := commitAgentFixesWithResult(f.s, types.StepReview, "repair", ""); err == nil {
				t.Fatal("rewritten head recorded")
			}
			stored, _ := f.s.DB.GetRun(f.s.Run.ID)
			if stored.HeadSHA != f.submitted {
				t.Fatal("rewrite acquired durable authority")
			}
			if got := gitCmd(t, f.fork, "rev-parse", "feature"); got != f.published {
				t.Fatal("rewrite published")
			}
		})
	}
}

func TestContinuation_MergeConflictNeverLaunchesResolver(t *testing.T) {
	f := newContinuationFixture(t)
	f.s.Agent = &mockAgent{name: "forbidden", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("resolver launched")
		return nil, nil
	}}
	_, err := (&CIStep{}).autoFixCI(f.s, &forgejoLogTestHost{}, &scm.PR{Number: "168", BaseBranch: "main"}, ciTargetsFor(nil, true))
	if !errors.Is(err, ErrHistoryConstraint) {
		t.Fatalf("err=%v", err)
	}
}

func TestContinuation_FFPublicationSamePRAndAdditiveRepairRevalidates(t *testing.T) {
	f := newContinuationFixture(t)
	recordReviewApproval(t, f.s, f.submitted)
	if err := publishRunHead(f.s, f.submitted, "", nil); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, f.fork, "rev-parse", "feature"); got != f.submitted {
		t.Fatal("wrong published head")
	}
	f.setPR(f.submitted, "main")
	if err := AssertContinuationPolicy(f.s); err != nil {
		t.Fatal(err)
	}
	recordCompletedReviewStep(t, f.s)
	out, err := (&PRStep{}).Execute(f.s)
	if err != nil || out.PRURL != fixtureExistingPR {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	log, _ := os.ReadFile(f.log)
	if strings.Contains(string(log), "pr create") || strings.Contains(string(log), "pr list") {
		t.Fatalf("duplicate discovery: %s", log)
	}
	writeFixtureFile(t, f.s.WorkDir, "repair.txt", "additive repair\n")
	repair, err := (&CIStep{}).commitRepair(f.s, "additive repair")
	if err != nil || !repair.HeadAdvanced || !repair.Revalidate {
		t.Fatalf("repair=%+v err=%v", repair, err)
	}
	gitCmd(t, f.s.WorkDir, "merge-base", "--is-ancestor", f.submitted, "HEAD")
	if got := gitCmd(t, f.fork, "rev-parse", "feature"); got != f.submitted {
		t.Fatal("repair published before revalidation")
	}
}

func TestContinuation_KnownRemoteNeverAuthorizesForceAndServerRejectsRace(t *testing.T) {
	f := newContinuationFixture(t)
	decision, err := historyPushDecision(f.s, resolvePushURL(f.s), "refs/heads/feature", f.submitted)
	if err != nil || !decision.fastForward {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	// Another writer lands after the decision. Plain push must let the server
	// refuse even when the prior remote was known, not acquire a fresh lease.
	gitCmd(t, f.s.WorkDir, "checkout", "--detach", f.published)
	gitCmd(t, f.s.WorkDir, "commit", "--allow-empty", "-m", "independent change")
	other := gitCmd(t, f.s.WorkDir, "rev-parse", "HEAD")
	gitCmd(t, f.s.WorkDir, "push", f.fork, other+":refs/heads/feature")
	gitCmd(t, f.s.WorkDir, "checkout", "feature")
	if err := stepGitPushCommit(f.s, resolvePushURL(f.s), f.submitted, "refs/heads/feature", "", false); err == nil {
		t.Fatal("server accepted non-FF race")
	}
	if _, err := historyPushDecision(f.s, resolvePushURL(f.s), "refs/heads/feature", f.submitted); err == nil {
		t.Fatal("divergent remote authorized")
	}
	if got := gitCmd(t, f.fork, "rev-parse", "feature"); got != other {
		t.Fatal("external head overwritten")
	}
}

func TestContinuation_PolicyLostOrMalformedFailsClosed(t *testing.T) {
	f := newContinuationFixture(t)
	for _, raw := range []string{"", `{}`, `{"version":2}`, `{"version":1,"pr_head":"main"}`} {
		f.s.Run.ContinuationJSON = &raw
		if err := AssertContinuationPolicy(f.s); err == nil {
			t.Fatalf("policy %q accepted", raw)
		}
	}
}

func TestContinuation_ExternalMovementDuringAgentIsRejected(t *testing.T) {
	f := newContinuationFixture(t)
	f.s.CheckRunPolicy = func() error { return AssertContinuationPolicy(f.s) }
	f.s.Agent = &mockAgent{name: "movement", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		gitCmd(t, f.upstream, "update-ref", "refs/heads/main", f.base)
		writeFixtureFile(t, opts.CWD, "repair.txt", "repair\n")
		return &agent.Result{Output: []byte(`{"summary":"repair"}`)}, nil
	}}
	if _, err := f.s.RunAgent(agent.RunOpts{CWD: f.s.WorkDir}); err == nil {
		t.Fatal("movement during repair accepted")
	}
	if _, err := os.Stat(filepath.Join(f.s.WorkDir, "repair.txt")); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, f.fork, "rev-parse", "feature"); got != f.published {
		t.Fatal("movement published")
	}
}

func TestContinuation_ForcedPublicationIsRefusedAtPushBoundary(t *testing.T) {
	f := newContinuationFixture(t)
	if err := stepGitPushCommit(f.s, resolvePushURL(f.s), f.submitted, "refs/heads/feature", f.published, true); err == nil {
		t.Fatal("known-head force-with-lease accepted")
	}
	if got := gitCmd(t, f.fork, "rev-parse", "feature"); got != f.published {
		t.Fatal("refused force changed remote")
	}
}

func TestContinuation_CIReadinessRequiresExactFinalHeadAndRealChecks(t *testing.T) {
	f := newContinuationFixture(t)
	recordReviewApproval(t, f.s, f.submitted)
	if err := publishRunHead(f.s, f.submitted, "", nil); err != nil {
		t.Fatal(err)
	}
	f.setPR(f.submitted, "main")
	if err := setCIMonitorReadiness(f.s, true, true); err == nil {
		t.Fatal("no-ci waiver counted as CI evidence")
	}
	if err := setCIMonitorReadiness(f.s, true, false); err != nil {
		t.Fatal(err)
	}
	run, _ := f.s.DB.GetRun(f.s.Run.ID)
	if run.CIReadyAt == nil {
		t.Fatal("exact-head CI not recorded")
	}
	if err := setCIMonitorReadiness(f.s, false, false); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, f.s.WorkDir, "commit", "--allow-empty", "-m", "later unvalidated head")
	if err := setCIMonitorReadiness(f.s, true, false); err == nil {
		t.Fatal("earlier checks certified a later head")
	}
	run, _ = f.s.DB.GetRun(f.s.Run.ID)
	if run.CIReadyAt != nil {
		t.Fatal("stale readiness recorded")
	}
}

func TestContinuation_ReadinessRefusalNeverLogsGreen(t *testing.T) {
	f := newContinuationFixture(t)
	var logs []string
	f.s.Log = func(line string) { logs = append(logs, line) }
	if message := logCIMonitorStatus(f.s, ciChecksPassedMsg, ""); message != "" {
		t.Fatal("unpublished head emitted green status")
	}
	for _, line := range logs {
		if line == ciChecksPassedMsg {
			t.Fatal("refused evidence logged as green")
		}
	}
	run, _ := f.s.DB.GetRun(f.s.Run.ID)
	if run.CIReadyAt != nil {
		t.Fatal("unpublished head marked ready")
	}
}
