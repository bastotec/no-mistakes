package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPublishTestPreview_RealPushSamePRPendingAttestation(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	gitCmd(t, dir, "push", upstream, "main")
	s := newTestContextWithDBRecords(t, prDraftAgent(), dir, base, head, config.Commands{})
	gitCmd(t, dir, "remote", "set-url", "origin", upstream)
	gate := setupGateMirror(t, s)
	gitCmd(t, gate, "fetch", dir, "feature:refs/heads/feature")
	recordReviewApproval(t, s, head)
	sr, err := s.DB.InsertStepResult(s.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.StartStep(sr.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[],"summary":"Waiting for live PR validation"}`
	if err := s.DB.ParkStepForApproval(s.Run.ID, sr.ID, types.StepStatusAwaitingApproval, 0, 0, &findings); err != nil {
		t.Fatal(err)
	}
	s.StepResultID = sr.ID
	bin := fakeCLIBinDir(t)
	linkTestBinary(t, bin, "gh")
	bodyFile := filepath.Join(t.TempDir(), "body")
	logFile := filepath.Join(t.TempDir(), "commands")
	s.Env = fakeCLIEnv(bin, map[string]string{"FAKE_CLI_MODE": "test-preview-gh", "FAKE_CLI_PR_BODY_FILE": bodyFile, "FAKE_CLI_PR_HEAD_SHA": head, "FAKE_CLI_LOG": logFile, "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "url." + upstream + ".insteadOf", "GIT_CONFIG_VALUE_0": "https://github.com/test/repo"})
	before, _ := s.DB.GetStepResult(sr.ID)
	for i := 0; i < 2; i++ {
		result, err := PublishTestPreview(s)
		if err != nil {
			t.Fatal(err)
		}
		if result.PRURL != "https://github.com/test/repo/pull/99" || result.PreviewURL != "https://preview.example.test/current" || result.TestStatus != "awaiting_approval" {
			t.Fatalf("result=%+v", result)
		}
	}
	after, _ := s.DB.GetStepResult(sr.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("Test evidence changed: %+v -> %+v", before, after)
	}
	if got := gitCmd(t, upstream, "rev-parse", "feature"); got != head {
		t.Fatalf("remote=%s want=%s", got, head)
	}
	run, _ := s.DB.GetRun(s.Run.ID)
	if run.LastPushedSHA == nil || *run.LastPushedSHA != head || run.PushGeneration == nil || *run.PushGeneration < 1 {
		t.Fatalf("missing durable custody binding: %+v", run)
	}
	body, _ := os.ReadFile(bodyFile)
	if result, out := runVerifyPy(t, string(body), head); result != "failure" {
		t.Fatalf("pending Test attestation passed verifier: %s", out)
	}
	log, _ := os.ReadFile(logFile)
	if strings.Count(string(log), "pr create ") != 1 || strings.Contains(string(log), "pr ready ") {
		t.Fatalf("not an idempotent draft publication: %s", log)
	}
	if _, err := s.DB.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 0); err != nil {
		t.Fatal(err)
	}
	for _, step := range []types.StepName{types.StepDocument, types.StepLint, types.StepPush, types.StepPR} {
		if _, err := s.DB.InsertStepResult(s.Run.ID, step); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DB.UpdateRunStatus(s.Run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	run, err = s.DB.GetRun(s.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	testAgent := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if !strings.Contains(opts.Prompt, "https://github.com/test/repo/pull/99") || !strings.Contains(opts.Prompt, head) {
			t.Error("post-publication Test did not receive its exact preview target")
		}
		attempts++
		if _, err := os.Stat(bodyFile + ".ready"); !os.IsNotExist(err) {
			t.Error("draft readied before post-publication Test")
		}
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	documentAgent := &mockAgent{name: "document", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(opts.CWD, "preview-notes.md"), []byte("Feature documentation\n"), 0600); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"document feature"}`)}, nil
	}}
	executor := pipeline.NewExecutor(s.DB, paths.WithRoot(t.TempDir()), s.Config, s.Agent, []pipeline.Step{
		&previewContinuationStep{Step: &TestStep{}, env: s.Env, gate: gate, ag: testAgent},
		&previewContinuationStep{Step: &DocumentStep{}, env: s.Env, gate: gate, ag: documentAgent},
		&previewContinuationStep{Step: &LintStep{}, env: s.Env, gate: gate, ag: documentAgent},
		&previewContinuationStep{Step: &PushStep{}, env: s.Env, gate: gate, ag: s.Agent},
		&previewContinuationStep{Step: &PRStep{}, env: s.Env, gate: gate, ag: s.Agent},
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() { defer close(finished); done <- executor.Resume(ctx, run, s.Repo, dir) }()
	t.Cleanup(func() { cancel(); <-finished })
	for {
		if err := executor.Respond(types.StepTest, types.ActionApprove, nil); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("recovered Test gate not available")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Test continuation did not complete")
	}
	if attempts != 1 {
		t.Fatalf("post-publication Test attempts=%d", attempts)
	}

	final, _ := os.ReadFile(bodyFile)
	if strings.Contains(string(final), "Test is still pending") {
		t.Fatal("stale preview prose survived normal publication")
	}
	if _, err := os.Stat(bodyFile + ".ready"); err != nil {
		t.Fatalf("validated preview not readied: %v", err)
	}
	publishedHead := gitCmd(t, upstream, "rev-parse", "feature")
	if publishedHead == head {
		t.Fatal("housekeeping did not advance the published head")
	}
	completedTest, err := s.DB.GetStepResult(sr.ID)
	if err != nil || completedTest == nil || completedTest.FindingsJSON == nil {
		t.Fatalf("completed Test=%+v, %v", completedTest, err)
	}
	findingsAfter, err := types.ParseFindingsJSON(*completedTest.FindingsJSON)
	if err != nil || findingsAfter.TestedHeadSHA != head {
		t.Fatalf("tested-head disclosure=%+v, %v", findingsAfter, err)
	}
	attestation := parsePipelineAttestationForTest(t, string(final))
	if attestation.HeadSHA != publishedHead || attestation.LiveValidation != nil {
		t.Fatalf("housekeeping publication claimed stale live validation: %+v", attestation)
	}
}

func TestTestPreview_CIRepairRevalidationUsesUnpublishedHead(t *testing.T) {
	fixture := newCIRepairFixture(t, true, writeCIFix)
	s := fixture.sctx
	if err := s.DB.BindTestPreview(s.Run.ID, *s.Run.PRURL); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.RecordTestPreviewValidation(s.Run.ID, s.Run.HeadSHA); err != nil {
		t.Fatal(err)
	}
	outcome, err := fixture.run(t)
	if err != nil || outcome == nil || outcome.RestartFrom != types.StepReview {
		t.Fatalf("CI repair did not request revalidation: %+v, %v", outcome, err)
	}
	repairHead := fixture.localHead(t)
	if repairHead == fixture.headSHA || fixture.remoteHead(t) != fixture.headSHA {
		t.Fatal("repair must remain unpublished until revalidation")
	}
	s.Ctx = context.Background()
	s.Fixing = false
	attempts := 0
	s.Agent = &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		attempts++
		if strings.Contains(opts.Prompt, "Run-owned PR validation preview:") {
			t.Error("repair revalidation requires an unpublished head's preview")
		}
		if !strings.Contains(opts.Prompt, repairHead) {
			t.Error("repair Test did not receive the current repair head")
		}
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	outcome, err = (&TestStep{}).Execute(s)
	if err != nil || outcome == nil || outcome.NeedsApproval || attempts != 1 {
		t.Fatalf("repair Test=%+v, %v; attempts=%d", outcome, err, attempts)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil || findings.TestedHeadSHA != repairHead {
		t.Fatalf("repair tested-head disclosure=%+v, %v", findings, err)
	}
}

func TestPublishTestPreview_RefusesDirtyBeforePush(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	s := newTestContextWithDBRecords(t, nil, dir, base, head, config.Commands{})
	if err := os.WriteFile(filepath.Join(dir, "uncommitted"), []byte("repair"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PublishTestPreview(s); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("error=%v", err)
	}
}

func TestPublishTestPreview_RefusesForeignOrReadyPRBeforePush(t *testing.T) {
	for _, kind := range []string{"foreign", "ready"} {
		t.Run(kind, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			s := newTestContextWithDBRecords(t, nil, dir, base, head, config.Commands{})
			upstream := t.TempDir()
			gitCmd(t, upstream, "init", "--bare")
			gitCmd(t, dir, "remote", "set-url", "origin", upstream)
			bin := fakeCLIBinDir(t)
			linkTestBinary(t, bin, "gh")
			body := filepath.Join(t.TempDir(), "body")
			log := filepath.Join(t.TempDir(), "log")
			content := "Author-owned PR"
			if kind == "ready" {
				content = testPreviewMarker(s.Run.ID)
				if err := os.WriteFile(body+".ready", []byte("ready"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(body, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			s.Env = fakeCLIEnv(bin, map[string]string{"FAKE_CLI_MODE": "test-preview-gh", "FAKE_CLI_PR_BODY_FILE": body, "FAKE_CLI_PR_HEAD_SHA": head, "FAKE_CLI_LOG": log})
			if _, err := PublishTestPreview(s); err == nil {
				t.Fatal("unsafe PR accepted")
			}
			if got := gitCmd(t, upstream, "for-each-ref", "refs/heads/"); got != "" {
				t.Fatalf("ref unexpectedly published: %s", got)
			}
			logged, _ := os.ReadFile(log)
			if strings.Contains(string(logged), "pr create ") || strings.Contains(string(logged), "pr edit ") {
				t.Fatalf("unsafe PR mutated: %s", logged)
			}
		})
	}
}

type previewContinuationStep struct {
	pipeline.Step
	env  []string
	gate string
	ag   agent.Agent
}

func (s *previewContinuationStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx.Env, ctx.GateDir, ctx.Agent = append(append([]string{}, s.env...), "FAKE_CLI_PR_HEAD_SHA="+ctx.Run.HeadSHA), s.gate, s.ag
	return s.Step.Execute(ctx)
}
