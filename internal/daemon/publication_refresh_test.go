package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// This is synthetic daemon/forge integration evidence, not live GitHub or
// product-validation evidence. Only a seeded CI worker is run: no launch RPC,
// nested pipeline, agent, managed service, or external network is involved.
func TestRefreshPublication_RPCMonitorJourney(t *testing.T) {
	binDir := t.TempDir()
	name := "gh"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	buildCtx, stopBuild := context.WithTimeout(context.Background(), time.Minute)
	defer stopBuild()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", filepath.Join(binDir, name), "../pipeline/fakecli")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build forge fixture: %v\n%s", err, out)
	}
	for _, scenario := range []string{"passed", "empty", "unreadable", "failed", "pending", "skipped", "run", "repo", "pr", "short-head", "recorded", "pushed", "remote", "forge", "source", "parked", "repairing"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("FAKE_CLI_MODE", "publication-refresh-gh")
			t.Setenv("NO_MISTAKES_TELEMETRY", "0")
			root, err := os.MkdirTemp("", "prpc") // short enough for Unix sockets
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(root) })
			t.Setenv("NM_HOME", root)
			t.Setenv("HOME", root)
			t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "no-global-config"))
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			p := paths.WithRoot(root)
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { d.Close() })
			work := filepath.Join(root, "work")
			remote := filepath.Join(root, "remote.git")
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(os.MkdirAll(work, 0o755))
			gitCmd(t, work, "init", "-b", "main")
			gitCmd(t, work, "config", "user.name", "Fixture")
			gitCmd(t, work, "config", "user.email", "fixture@example.test")
			gitCmd(t, work, "commit", "--allow-empty", "-m", "reviewed head")
			oldHead := gitOutput(t, work, "rev-parse", "HEAD")
			gitCmd(t, work, "checkout", "-b", "feature")
			gitCmd(t, work, "commit", "--allow-empty", "-m", "synthetic published CI repair")
			head := gitOutput(t, work, "rev-parse", "HEAD")
			gitCmd(t, work, "init", "--bare", remote)
			// Keep the GitHub source identity, but route all Git transport to
			// this disposable local remote, never to the network.
			gitCmd(t, work, "config", "url."+remote+".insteadOf", "https://github.com/test/repo")
			gitCmd(t, work, "remote", "add", "origin", "https://github.com/test/repo")
			gitCmd(t, work, "push", "origin", "main", "feature")
			repo, err := d.InsertRepoWithID("refresh-fixture", work, "https://github.com/test/repo", "main")
			must(err)
			// An explicit push URL keeps the forge source identity separate
			// from Git's local insteadOf transport expansion.
			repo.ForkURL = "https://github.com/test/repo"
			run, err := d.InsertRun(repo.ID, "feature", head, oldHead)
			must(err)
			url := "https://github.com/test/repo/pull/42"
			must(d.UpdateRunPRURL(run.ID, url))
			must(d.UpdateRunReviewApprovedHeadSHA(run.ID, oldHead))
			binding := db.PushBinding{HeadSHA: head, TargetKind: "upstream", TargetFingerprint: "fixture", Ref: "refs/heads/feature"}
			must(d.UpdateRunPushBinding(run.ID, binding))
			review, err := d.InsertStepResult(run.ID, types.StepReview)
			must(err)
			findings := `{"summary":"R1 remains unproven","risk_level":"high","items":[{"id":"R1","severity":"error","description":"unresolved historical concern","action":"ask-user"}]}`
			must(d.SetStepFindings(review.ID, findings))
			_, err = d.InsertReviewStepRound(review.ID, 1, "initial", &findings, nil, oldHead, 99)
			must(err)
			must(d.CompleteSkippedStep(review.ID, 0, 99, "", "historical skip"))
			test, err := d.InsertStepResult(run.ID, types.StepTest)
			must(err)
			must(d.FailStep(test.ID, "historical timeout", 100))
			// Seed the public serialized attestation contract, not source text.
			original := "Author narrative. High risk: R1 remains unproven. Test timed out.\n"
			original += "<!-- no-mistakes-pipeline-attestation:v1 " + `{"head_sha":"` + head + `","steps":[{"step":"review","status":"skipped"},{"step":"test","status":"failed"}],"review":{"effort":"high","reason":"historical review"}}` + " -->"
			bodyPath := filepath.Join(root, "body")
			must(os.WriteFile(bodyPath, []byte(original), 0o600))
			t.Setenv("FAKE_CLI_PR_BODY_FILE", bodyPath)
			t.Setenv("FAKE_CLI_LOG", filepath.Join(root, "forge.log"))
			t.Setenv("FAKE_CLI_STATE", "OPEN")
			t.Setenv("FAKE_CLI_PR_BASE", "main")
			t.Setenv("FAKE_CLI_PR_HEAD_SHA", head)
			forgeHead, source := head, "feature"
			if scenario == "forge" {
				forgeHead = oldHead
			}
			if scenario == "source" {
				source = "other"
			}
			identity := map[string]interface{}{
				"number": 42, "html_url": url, "state": "open", "merged": false,
				"base": map[string]interface{}{"ref": "main", "repo": map[string]string{"full_name": "test/repo", "html_url": "https://github.com/test/repo"}},
				"head": map[string]interface{}{"ref": source, "sha": forgeHead, "repo": map[string]string{"full_name": "test/repo", "html_url": "https://github.com/test/repo"}},
			}
			payload, err := json.Marshal(identity)
			must(err)
			t.Setenv("FAKE_CLI_EXISTING_PR_JSON", string(payload))
			bucket, state, want := "pass", "SUCCESS", "all reported live CI checks passed"
			switch scenario {
			case "empty":
				want = "inconclusive"
			case "unreadable":
				t.Setenv("FAKE_CLI_REFRESH_ERROR", "1")
				want = "inconclusive"
			case "failed":
				bucket, state, want = "fail", "FAILURE", "unresolved"
			case "pending":
				bucket, state, want = "pending", "IN_PROGRESS", "unresolved"
			case "skipped":
				bucket, state, want = "skipping", "SKIPPED", "unresolved"
			}
			checks := []map[string]string{{"name": "test", "bucket": bucket, "state": state}}
			if scenario == "empty" {
				checks = nil
			}
			payload, err = json.Marshal(checks)
			must(err)
			t.Setenv("FAKE_CLI_REFRESH_CHECKS", string(payload))
			run, err = d.GetRun(run.ID)
			must(err)
			cfg := config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{})
			cfg.CITimeout = time.Minute
			monitor := (&steps.CIStep{}).SetPollIntervalOverride(time.Minute).SetBaseBranchTip(func(context.Context) (string, bool) { return oldHead, true })
			ready := make(chan struct{}, 1)
			executor := pipeline.NewExecutor(d, p, cfg, nil, []pipeline.Step{monitor}, func(event ipc.Event) {
				if event.Content != nil && strings.Contains(*event.Content, "CI checks running, waiting for results...") {
					select {
					case ready <- struct{}{}:
					default:
					}
				}
			})
			mgr := NewRunManager(d, p, nil)
			mgr.executors[run.ID] = executor
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			done := make(chan error, 1)
			go func() { done <- executor.Execute(ctx, run, repo, work) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("fixture CI worker did not stop")
				}
			})
			select {
			case <-ready:
			case err := <-done:
				done <- err
				t.Fatalf("monitor stopped before readiness: %v", err)
			case <-ctx.Done():
				t.Fatal("monitor never became ready")
			}
			rows, err := d.GetStepsByRun(run.ID)
			must(err)
			for _, row := range rows {
				if row.StepName == types.StepCI {
					summary := "synthetic repair published"
					_, err := d.InsertStepRoundWithRepair(row.ID, 2, "auto_fix", nil, &summary, true, 42)
					must(err)
				}
			}
			params := ipc.RefreshPublicationParams{RepoID: repo.ID, RunID: run.ID, PRURL: url, HeadSHA: head}
			switch scenario {
			case "run":
				params.RunID = "absent"
			case "repo":
				params.RepoID = "absent"
			case "pr":
				params.PRURL += "0"
			case "short-head":
				params.HeadSHA = head[:8]
			case "recorded":
				must(d.UpdateRunHeadSHA(run.ID, oldHead))
			case "pushed":
				binding.HeadSHA = oldHead
				must(d.UpdateRunPushBinding(run.ID, binding))
			case "remote":
				gitCmd(t, work, "push", "--force", "origin", oldHead+":refs/heads/feature")
			case "parked":
				must(d.SetRunAwaitingAgent(run.ID))
			case "repairing":
				for _, row := range rows {
					if row.StepName == types.StepCI {
						must(d.UpdateStepStatus(row.ID, types.StepStatusFixing))
					}
				}
			}
			snapshot := func() string {
				r, err := d.GetRun(run.ID)
				must(err)
				// The still-active monitor legitimately records another OPEN
				// observation after serving a refresh. Compare durable history
				// and authority, not these two polling wall-clock timestamps.
				r.PRStateObservedAt = nil
				r.UpdatedAt = 0
				s, err := d.GetStepsByRun(run.ID)
				must(err)
				rounds := map[string][]*db.StepRound{}
				for _, row := range s {
					rounds[row.ID], err = d.GetRoundsByStep(row.ID)
					must(err)
				}
				data, err := json.Marshal([]interface{}{r, s, rounds})
				must(err)
				return string(data)
			}
			before := snapshot()
			refsBefore := gitOutput(t, work, "ls-remote", "origin")
			srv := ipc.NewServer()
			registerHandlers(srv, mgr, d, func() { t.Error("refresh shut down fixture daemon") })
			must(srv.Listen(p.Socket()))
			served := make(chan error, 1)
			go func() { served <- srv.ServeReady() }()
			t.Cleanup(func() {
				srv.Close()
				select {
				case err := <-served:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(5 * time.Second):
					t.Error("fixture IPC server did not stop")
				}
			})
			client, err := ipc.Dial(p.Socket())
			must(err)
			t.Cleanup(func() { client.Close() })
			var result ipc.RefreshPublicationResult
			err = client.CallWithContext(ctx, ipc.MethodRefreshPublication, params, &result, 10*time.Second)
			body, readErr := os.ReadFile(bodyPath)
			must(readErr)
			reason, refused := map[string]string{
				"run": "active CI executor",
				"repo": "exact repository, PR and current head",
				"pr": "exact repository, PR and current head",
				"short-head": "exact repository, PR and current head",
				"recorded": "exact repository, PR and current head",
				"pushed": "recorded/pushed head binding changed",
				"remote": "remote source head does not match",
				"forge": "forge PR identity/head mismatch",
				"source": "source repository/ref does not match",
				"parked": "not a repair or approval gate",
				"repairing": "not a repair or approval gate",
			}[scenario]
			if refused {
				if err == nil || !strings.Contains(err.Error(), reason) || string(body) != original {
					t.Fatalf("binding mismatch published: result=%+v err=%v body=%s", result, err, body)
				}
			} else {
				must(err)
				if result.RunID != params.RunID || result.HeadSHA != head || result.PRURL != url || !strings.HasPrefix(result.Disposition, want) {
					t.Fatalf("wrong attributed response: %+v", result)
				}
				if !strings.HasPrefix(string(body), original) || !strings.Contains(string(body), "Attributed to the CI monitor") || !strings.Contains(string(body), "No finding is certified fixed by green CI") || !strings.Contains(string(body), "Recorded published CI repair round 2") {
					t.Fatalf("history or attribution lost: %s", body)
				}
				if scenario != "passed" && strings.Contains(string(body), "all reported live CI checks passed") {
					t.Fatalf("unsupported passing claim: %s", body)
				}
			}
			after := snapshot()
			if before != after || refsBefore != gitOutput(t, work, "ls-remote", "origin") {
				t.Fatalf("refresh changed durable run/steps/rounds or remote history\nbefore=%s\nafter=%s", before, after)
			}
			// The mock's argv transcript is an executed boundary contract:
			// only reads and one body-only edit are authorized. In particular,
			// no validation, workflow rerun, label or merge mutation is allowed,
			// even if production were to ignore the command's failure.
			transcript, err := os.Open(bodyPath + ".commands")
			must(err)
			defer transcript.Close()
			scanner := bufio.NewScanner(transcript)
			edits := 0
			for scanner.Scan() {
				var args []string
				must(json.Unmarshal(scanner.Bytes(), &args))
				if len(args) < 2 {
					t.Fatalf("unexpected forge command: %q", args)
				}
				switch {
				case args[0] == "auth" && args[1] == "status":
				case args[0] == "pr" && args[1] == "view":
				case len(args) > 3 && reflect.DeepEqual(args[:4], []string{"api", "--hostname", "github.com", "graphql"}):
					if !strings.Contains(strings.Join(args, " "), "query=query(") {
						t.Fatalf("unexpected GraphQL mutation: %q", args)
					}
				case len(args) > 5 && reflect.DeepEqual(args[:6], []string{"api", "--hostname", "github.com", "--method", "GET", "repos/test/repo/actions/runs"}):
				case reflect.DeepEqual(args, []string{"pr", "edit", "42", "--repo", "test/repo", "--body-file", "-"}):
					edits++
				default:
					t.Fatalf("unexpected forge mutation: %q", args)
				}
			}
			must(scanner.Err())
			if (refused && edits != 0) || (!refused && edits != 1) {
				t.Fatalf("forge body edits=%d, refused=%t", edits, refused)
			}
			t.Logf("synthetic RPC/CI monitor scenario %s: disposition=%q refused=%t; history unchanged", scenario, result.Disposition, refused)
			if scenario == "passed" {
				t.Logf("synthetic RPC response: %+v\nMock-forge published PR body:\n%s", result, body)
			}
		})
	}
}
