package steps

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
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
	findings := `{"findings":[],"summary":"Waiting for live PR validation"}`
	if err := s.DB.ParkStepForApproval(s.Run.ID, sr.ID, types.StepStatusAwaitingApproval, 1, 0, &findings); err != nil {
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
	// The ordinary PR phase replaces only the owned pending narrative and readies
	// the SAME PR after Test actually completes, not during the preview request.
	if err := s.DB.CompleteStep(sr.ID, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	_, err = (&PRStep{}).Execute(s)
	if err != nil {
		t.Fatal(err)
	}
	final, _ := os.ReadFile(bodyFile)
	if strings.Contains(string(final), "Test is still pending") {
		t.Fatal("stale preview prose survived normal publication")
	}
	if _, err := os.Stat(bodyFile + ".ready"); err != nil {
		t.Fatalf("validated preview not readied: %v", err)
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
