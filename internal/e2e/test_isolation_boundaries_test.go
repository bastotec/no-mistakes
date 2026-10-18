//go:build e2e && linux

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// These are running-product journeys: a disposable daemon dispatches the real
// Test worker. Only Deck/model responses and adversarial inputs are synthetic.
// No provider, application, shared service or scheduler-timed kill is involved.
func TestIsolatedTestNestedRepositoryJourney(t *testing.T) {
	for _, kind := range []string{"gitlink", "untracked", "git-file", "bare"} {
		t.Run(kind, func(t *testing.T) {
			h, requests := newTestBoundaryHarness(t, kind, false)
			run := launchTestBoundary(t, h, true, false)
			want := "nested Git metadata is unsupported"
			if kind == "gitlink" {
				want = "gitlinks/submodules are unsupported"
			} else if kind == "bare" {
				want = "nested bare repository is unsupported"
			}
			if run.Status != types.RunFailed || run.Error == nil || !strings.Contains(*run.Error, "test-isolation-repository: "+want) {
				t.Fatalf("nested layout was not refused by Test: %+v", run)
			}
			if requests.Load() != 1 {
				t.Fatalf("canary did not run through the worker/model bridge: %d", requests.Load())
			}
			work := paths.WithRoot(h.NMHome).WorktreeDir(run.RepoID, run.ID)
			assertBoundaryFile(t, filepath.Join(work, "canary-created"), []byte(kind))
			if run.SurvivedWork == nil || run.SurvivedWork.State != types.SurvivedWorkInPlace {
				t.Fatalf("unsupported layout lost custody: %+v", run.SurvivedWork)
			}
			assertNoHostCallback(t, h)
			t.Logf("live %s canary refused; worker ran; unsupported work retained; zero host callback markers", kind)
		})
	}
}

func TestIsolatedTestNormalizationJourney(t *testing.T) {
	for _, mode := range []string{"autocrlf", "attributes"} {
		t.Run(mode, func(t *testing.T) {
			h, requests := newTestBoundaryHarness(t, "normalization", true)
			files := map[string]string{"normalized.txt": "first line\nsecond line\n", "binary.dat": "binary\x00\nunchanged\n"}
			if mode == "attributes" {
				files["info.dat"] = "info normalized\n"
				files["precedence.txt"] = "info takes precedence\n"
				files["repo.txt"] = "repository takes precedence\n"
				files[".gitattributes"] = "repo.txt text eol=lf\n"
			}
			for name, content := range files {
				h.CommitChange("main", name, content, "add normalization fixture")
			}
			publishTestBoundary(t, h)
			config := "[core]\nautocrlf = true\n"
			if mode == "attributes" {
				external := filepath.Join(h.HomeDir, "attributes")
				writeBoundaryFile(t, external, "*.txt text eol=crlf\nprecedence.txt -text\n", 0600)
				config = "[core]\nautocrlf = false\nattributesFile = " + external + "\n"
			}
			writeBoundaryFile(t, filepath.Join(h.HomeDir, ".gitconfig"), config, 0600)
			initFromOwnWorktree(t, h, "initialize-boundary")
			gate := paths.WithRoot(h.NMHome).RepoDir(h.repoID())
			if mode == "attributes" {
				writeBoundaryFile(t, filepath.Join(gate, "info", "attributes"), "info.dat text eol=crlf\nprecedence.txt text eol=crlf\n", 0600)
			}
			run := launchPreparedTestBoundary(t, h, true, true)
			if run.Status != types.RunCompleted || run.SubmittedHeadSHA == nil || run.HeadSHA != *run.SubmittedHeadSHA {
				t.Fatalf("no-op isolated fixer changed HEAD or failed: %+v", run)
			}
			if requests.Load() != 3 {
				t.Fatalf("expected initial evidence, no-op repair and final evidence: %d", requests.Load())
			}
			for name, want := range files {
				got := boundaryGit(t, h, gate, "show", run.HeadSHA+":"+name)
				if !bytes.Equal(got, []byte(want)) {
					t.Fatalf("committed blob %s changed: %q, want %q", name, got, want)
				}
			}
			// The fixture asserts actual checkout CRLF/LF/binary bytes inside
			// both worker invocations, not merely the final blob identities.
			t.Logf("live no-op %s repair completed; original HEAD and all LF/binary blobs preserved", mode)
		})
	}
}

func TestIsolatedTestFailureCustodyJourney(t *testing.T) {
	for _, kind := range []string{"terminal", "timeout"} {
		t.Run(kind, func(t *testing.T) {
			h, requests := newTestBoundaryHarness(t, kind, true)
			// The inherited callbacks point at files only Test will write.
			// They are absent during setup and ordinary pre-Test steps.
			writeBoundaryFile(t, filepath.Join(h.HomeDir, ".gitconfig"), "[core]\nhooksPath = .githooks\nfsmonitor = .githooks/monitor\n[filter \"custom\"]\nclean = sh .githooks/filter\n", 0600)
			run := launchTestBoundary(t, h, true, true)
			if run.Status != types.RunFailed || run.Error == nil {
				t.Fatalf("fixture did not fail through terminalization: %+v", run)
			}
			gate := paths.WithRoot(h.NMHome).RepoDir(run.RepoID)
			if kind == "terminal" {
				if !strings.Contains(*run.Error, "fixture evidence failure") || run.SubmittedHeadSHA == nil || run.HeadSHA == *run.SubmittedHeadSHA {
					t.Fatalf("expected evidence failure after a committed repair: %+v", run)
				}
				anchor := strings.TrimSpace(string(boundaryGit(t, h, gate, "rev-parse", custody.RecoveryRef(run.ID))))
				if anchor != run.HeadSHA {
					t.Fatalf("unpublished terminal head lost: %s != %s", anchor, run.HeadSHA)
				}
				assertBoundaryBlob(t, h, gate, anchor+":repaired.txt", "isolated repair fixture\n")
			} else {
				if !strings.Contains(*run.Error, "timed out") || run.SurvivedWork == nil || run.SurvivedWork.State != types.SurvivedWorkCaptured {
					t.Fatalf("timed-out worker lost uncommitted custody: %+v", run)
				}
				assertBoundaryBlob(t, h, gate, run.SurvivedWork.Ref+":leftover.txt", "uncommitted Test work\n")
				assertBoundaryBlob(t, h, gate, run.SurvivedWork.Ref+":.githooks/reference-transaction", boundaryCallback(h, false))
			}
			// Run completion is emitted before cleanup; wait for the actual
			// daemon worktree removal before checking the host callback marker.
			work := paths.WithRoot(h.NMHome).WorktreeDir(run.RepoID, run.ID)
			deadline := time.Now().Add(10 * time.Second)
			for {
				_, err := os.Stat(work)
				if os.IsNotExist(err) {
					break
				}
				if time.Now().After(deadline) {
					h.dumpDebugState()
					t.Fatalf("terminal cleanup did not remove worktree: %v", err)
				}
				time.Sleep(25 * time.Millisecond)
			}
			assertNoHostCallback(t, h)
			want := int32(3)
			if kind == "timeout" {
				want = 2
			}
			if requests.Load() != want {
				t.Fatalf("wrong worker/model execution count: %d, want %d", requests.Load(), want)
			}
			t.Logf("live %s failure preserved %s custody and completed daemon cleanup with zero host callback markers", kind, run.HeadSHA)
		})
	}
}

func TestOrdinaryTestBoundaryJourney(t *testing.T) {
	h, requests := newTestBoundaryHarness(t, "ordinary", false)
	writeBoundaryFile(t, filepath.Join(h.HomeDir, ".gitconfig"), "[core]\nhooksPath = .githooks\n", 0600)
	h.CommitChange("main", ".githooks/reference-transaction", boundaryCallback(h, false), "add ordinary callback control")
	boundaryGit(t, h, h.WorkDir, "update-index", "--chmod=+x", ".githooks/reference-transaction")
	boundaryGit(t, h, h.WorkDir, "commit", "-m", "make ordinary hook executable")
	run := launchTestBoundary(t, h, false, false)
	if run.Status != types.RunCompleted {
		t.Fatalf("ordinary fixture failed: %+v", run)
	}
	for _, name := range []string{"ordinary-prepare", "ordinary-baseline", "ordinary-agent", "host-callback"} {
		data, err := os.ReadFile(filepath.Join(h.HomeDir, name))
		if err != nil || len(data) == 0 {
			t.Fatalf("ordinary control %s did not execute: %q %v", name, data, err)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("ordinary fixture unexpectedly used the private model bridge")
	}
	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	isolated, err := database.TestIsolationEnabled(run.ID)
	if err != nil || isolated {
		t.Fatalf("ordinary run acquired isolation: %v %v", isolated, err)
	}
	t.Log("live ordinary fixture completed: ambient sentinels, host hook and ordinary completion retained")
}

func newTestBoundaryHarness(t *testing.T, kind string, repair bool) (*Harness, *atomic.Int32) {
	t.Helper()
	if kind != "ordinary" {
		if os.Getenv("NM_TEST_POSTGRES_ROOT") == "" {
			t.Skip("set NM_TEST_POSTGRES_ROOT for native isolated Test journeys")
		}
		if _, err := exec.LookPath("bwrap"); err != nil {
			t.Skip("bubblewrap unavailable")
		}
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("Python fixture unavailable")
	}
	requests := &atomic.Int32{}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer fixture-only" {
			t.Errorf("unexpected fixture model authority: %s %v", r.URL.Path, r.Header)
			http.Error(w, "refused", 403)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: fixture-model-response\n\n")
	}))
	t.Cleanup(model.Close)
	extra := ""
	if kind == "timeout" {
		extra = "test_agent_timeout: 5s"
	}
	h := NewHarness(t, SetupOpts{Agent: "deck", GlobalConfigExtra: extra})
	key := filepath.Join(h.HomeDir, "model-key")
	writeBoundaryFile(t, key, "fixture-only", 0600)
	t.Setenv("NM_TEST_MODEL_URL", model.URL+"/v1")
	t.Setenv("NM_TEST_MODEL_KEY_FILE", key)
	t.Setenv("NM_TEST_MODEL", "fixture-model")
	t.Setenv("UNRECOGNIZED_CREDENTIAL", "ambient-sentinel")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-sentinel")
	// Reuse the whole-step fixture's real PostgreSQL and local model probes.
	// The egress probe uses a reserved TEST-NET endpoint, never a provider.
	script := strings.ReplaceAll(isolatedDeckFixture, "HOST_CANARY", "127.0.0.1:1")
	script = strings.Replace(script, "result={'findings'", boundaryDeckActions+"\nresult={'findings'", 1)
	script = strings.ReplaceAll(script, "BOUNDARY_KIND", kind)
	script = strings.ReplaceAll(script, "HOST_CALLBACK_SCRIPT", fmt.Sprintf("%q", boundaryCallback(h, false)))
	script = strings.ReplaceAll(script, "HOST_FILTER_SCRIPT", fmt.Sprintf("%q", boundaryCallback(h, true)))
	script = strings.ReplaceAll(script, "ORDINARY_HOME", fmt.Sprintf("%q", h.HomeDir))
	if kind == "normalization" {
		script = strings.Replace(script, "open('repaired.txt','w').write('isolated repair fixture\\n')", "open('.fixture-repaired','w').write('no-op repair ran\\n')", 1)
	}
	writeBoundaryFile(t, filepath.Join(h.BinDir, "deck"), script, 0700)
	config := "allow_repo_commands: true\ncommands:\n  test: 'test -z \"$AWS_SECRET_ACCESS_KEY\" && test -n \"$NM_TEST_DATABASE_URL\"'\n"
	if repair {
		marker := "repaired.txt"
		if kind == "normalization" {
			marker = ".fixture-repaired"
			h.CommitChange("main", ".gitignore", ".fixture-repaired\n", "ignore no-op repair marker")
		}
		config = strings.Replace(config, "$NM_TEST_DATABASE_URL\"'", "$NM_TEST_DATABASE_URL\" && test -f "+marker+"'", 1)
	}
	if kind == "ordinary" {
		probe := `test "$AWS_SECRET_ACCESS_KEY" = ambient-sentinel && test "$UNRECOGNIZED_CREDENTIAL" = ambient-sentinel && test -z "$NM_TEST_DATABASE_URL"`
		marker := filepath.Join(h.HomeDir, "host-callback")
		config = fmt.Sprintf("allow_repo_commands: true\ncommands:\n  prepare: '%s && git config --worktree core.hooksPath .githooks && printf prepared > %s'\n  test: '%s && rm -f %s && git update-ref refs/fixture/ordinary HEAD && test -s %s && printf tested > %s'\n", probe, filepath.Join(h.HomeDir, "ordinary-prepare"), probe, marker, marker, filepath.Join(h.HomeDir, "ordinary-baseline"))
	}
	h.CommitChange("main", ".no-mistakes.yaml", config, "configure synthetic Test boundary")
	return h, requests
}

func publishTestBoundary(t *testing.T, h *Harness) {
	t.Helper()
	boundaryGit(t, h, h.WorkDir, "push", "origin", "main")
}

func launchTestBoundary(t *testing.T, h *Harness, isolated, repair bool) *ipc.RunInfo {
	t.Helper()
	publishTestBoundary(t, h)
	initFromOwnWorktree(t, h, "initialize-boundary")
	return launchPreparedTestBoundary(t, h, isolated, repair)
}

func launchPreparedTestBoundary(t *testing.T, h *Harness, isolated, repair bool) *ipc.RunInfo {
	t.Helper()
	const branch = "feature/test-boundary"
	h.CommitChange(branch, "feature.txt", "synthetic boundary change\n", "exercise Test boundary")
	operator := h.AddWorktree(branch)
	args := []string{"axi", "run", "--intent", "exercise synthetic local Test boundary only"}
	if isolated {
		args = append(args, "--isolate-test")
	}
	output, err := h.RunInDir(operator, args...)
	if repair {
		parked := h.ActiveRun(branch)
		if err != nil || parked == nil || !strings.Contains(output, "gate:") {
			t.Fatalf("expected synthetic Test baseline gate: %v\n%s", err, output)
		}
		step, ok := findStep(h.RunInfo(parked.ID).Steps, types.StepTest)
		if !ok || step.FindingsJSON == nil {
			t.Fatalf("Test findings missing: %+v", step)
		}
		findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, item := range findings.Items {
			if item.Category == types.FindingCategoryTestCommand {
				ids = append(ids, item.ID)
			}
		}
		if len(ids) != 1 {
			t.Fatalf("expected one baseline repair target: %+v", findings)
		}
		// Git copies the gate's pinned hooksPath into new worktrees, so
		// a global setting alone does not arm a descendant-written hook.
		// Change only this disposable run's host-side worktree policy before
		// its authorized repair; the private worker must not import it.
		work := paths.WithRoot(h.NMHome).WorktreeDir(parked.RepoID, parked.ID)
		boundaryGit(t, h, work, "config", "--worktree", "core.hooksPath", ".githooks")
		h.RespondToFindings(parked.ID, types.StepTest, types.ActionFix, ids)
	}
	run := h.WaitForRun(branch, 30*time.Second)
	step, ok := findStep(run.Steps, types.StepTest)
	if !ok || (step.Status != types.StepStatusFailed && step.Status != types.StepStatusCompleted) {
		t.Fatalf("Test did not settle: %+v\nlaunch error: %v\n%s", step, err, output)
	}
	return run
}

func boundaryGit(t *testing.T, h *Harness, dir string, args ...string) []byte {
	t.Helper()
	out, err := h.runGit(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("fixture git %v: %v\n%s", args, err, out)
	}
	return out
}

func writeBoundaryFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func assertBoundaryFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("fixture bytes at %s = %q, want %q: %v", path, got, want, err)
	}
}

func assertBoundaryBlob(t *testing.T, h *Harness, gate, ref, want string) {
	t.Helper()
	if got := boundaryGit(t, h, gate, "show", ref); !bytes.Equal(got, []byte(want)) {
		t.Fatalf("preserved bytes at %s = %q, want %q", ref, got, want)
	}
}

func assertNoHostCallback(t *testing.T, h *Harness) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(h.HomeDir, "host-callback")); !os.IsNotExist(err) {
		t.Fatalf("Test-written callback executed on host: %v", err)
	}
}

func boundaryCallback(h *Harness, filter bool) string {
	result := "#!/bin/sh\nif [ \"$UNRECOGNIZED_CREDENTIAL\" = ambient-sentinel ]; then printf called >> " + shellQuote(filepath.Join(h.HomeDir, "host-callback")) + "; fi\n"
	if filter {
		result += "cat\n"
	}
	return result
}

const boundaryDeckActions = `kind='BOUNDARY_KIND'
isolated=bool(os.environ.get('NM_TEST_DATABASE_URL'))
fixing='Fix the failing tests in this repository.' in prompt
if isolated and kind in ('gitlink','untracked','git-file','bare'):
 args=['git','init']
 if kind=='bare': args+=['--bare']
 if kind=='git-file': args+=['--separate-git-dir','/tmp/nested-metadata']
 subprocess.check_call(args+['nested'],stdout=subprocess.DEVNULL)
 if kind!='bare':
  open('nested/tracked','w').write('original\n')
  subprocess.check_call(['git','-C','nested','add','tracked'])
  subprocess.check_call(['git','-C','nested','commit','-qm','nested fixture'])
  if kind=='gitlink':
   head=subprocess.check_output(['git','-C','nested','rev-parse','HEAD']).decode().strip()
   subprocess.check_call(['git','update-index','--add','--cacheinfo','160000,'+head+',nested'])
  open('nested/.gitattributes','w').write('tracked filter=custom\n')
  open('nested/tracked','w').write('modified\n')
 callbacks=os.path.abspath('nested-callbacks')
 os.mkdir(callbacks)
 for name in ('monitor','post-index-change','reference-transaction'):
  path=callbacks+'/'+name
  open(path,'w').write(HOST_CALLBACK_SCRIPT)
  os.chmod(path,0o700)
 open(callbacks+'/filter','w').write(HOST_FILTER_SCRIPT)
 for key,value in [('core.fsmonitor',callbacks+'/monitor'),('core.hooksPath',callbacks),('filter.custom.clean','sh '+callbacks+'/filter')]:
  subprocess.check_call(['git','-C','nested','config',key,value])
 open('canary-created','w').write(kind)
if isolated and kind=='normalization':
 assert open('normalized.txt','rb').read()==b'first line\r\nsecond line\r\n'
 assert open('binary.dat','rb').read()==b'binary\x00\nunchanged\n'
 if os.path.exists('info.dat'):
  assert open('info.dat','rb').read()==b'info normalized\r\n'
  assert open('precedence.txt','rb').read()==b'info takes precedence\r\n'
  assert open('repo.txt','rb').read()==b'repository takes precedence\n'
if isolated and ((kind=='timeout' and fixing) or (kind=='terminal' and not fixing and os.path.exists('repaired.txt'))):
 os.makedirs('.githooks',exist_ok=True)
 for name in ('monitor','post-index-change','reference-transaction'):
  path='.githooks/'+name
  open(path,'w').write(HOST_CALLBACK_SCRIPT)
  os.chmod(path,0o700)
 open('.githooks/filter','w').write(HOST_FILTER_SCRIPT)
 open('.gitattributes','w').write('leftover.txt filter=custom\n')
 open('leftover.txt','w').write('uncommitted Test work\n')
 if kind=='timeout':
  import time
  time.sleep(30)
 else:
  print('fixture evidence failure',file=sys.stderr)
  sys.exit(1)
if kind=='ordinary' and os.path.exists(ORDINARY_HOME+'/ordinary-baseline'):
 assert not isolated
 assert os.environ['AWS_SECRET_ACCESS_KEY']=='ambient-sentinel'
 assert os.environ['UNRECOGNIZED_CREDENTIAL']=='ambient-sentinel'
 open(ORDINARY_HOME+'/ordinary-agent','w').write('ambient environment retained')
`
