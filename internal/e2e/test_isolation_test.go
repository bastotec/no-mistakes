//go:build e2e && linux

package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// This journey starts the real daemon and Test worker, but its model endpoint
// and Deck process are deterministic fixtures: no provider or application run.
func TestIsolatedTestWholeStepJourney(t *testing.T) {
	runIsolatedTestJourney(t, false)
}

func TestIsolatedTestRecoveryAndRepairJourney(t *testing.T) {
	runIsolatedTestJourney(t, true)
}

func runIsolatedTestJourney(t *testing.T, repair bool) {
	t.Helper()
	if os.Getenv("NM_TEST_POSTGRES_ROOT") == "" {
		t.Skip("set NM_TEST_POSTGRES_ROOT for the native PostgreSQL isolated-daemon journey")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap unavailable")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("Python fixture unavailable")
	}
	var modelRequests atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer fixture-only" {
			t.Errorf("unexpected model authority: %s %v", r.URL.Path, r.Header)
			http.Error(w, "refused", 403)
			return
		}
		modelRequests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: fixture-model-response\n\n")
	}))
	defer model.Close()
	canary, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer canary.Close()
	h := NewHarness(t, SetupOpts{Agent: "deck"})
	key := filepath.Join(h.HomeDir, "model-key")
	if err := os.WriteFile(key, []byte("fixture-only"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_TEST_MODEL_URL", model.URL+"/v1")
	t.Setenv("NM_TEST_MODEL_KEY_FILE", key)
	t.Setenv("NM_TEST_MODEL", "fixture-model")
	t.Setenv("UNRECOGNIZED_CREDENTIAL", "ambient-sentinel")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-sentinel")
	script := strings.ReplaceAll(isolatedDeckFixture, "HOST_CANARY", canary.Addr().String())
	if err := os.WriteFile(filepath.Join(h.BinDir, "deck"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	// Baseline and prepare are inside the boundary too; not only the agent.
	config := `allow_repo_commands: true
commands:
  prepare: 'test -z "$UNRECOGNIZED_CREDENTIAL" && test -n "$NM_TEST_DATABASE_URL"'
  test: 'test -z "$AWS_SECRET_ACCESS_KEY" && test -n "$NM_TEST_DATABASE_URL"'
`
	if repair {
		config = strings.Replace(config, `test -z "$AWS_SECRET_ACCESS_KEY" && test -n "$NM_TEST_DATABASE_URL"`, `test -z "$AWS_SECRET_ACCESS_KEY" && test -n "$NM_TEST_DATABASE_URL" && test -f repaired.txt`, 1)
	}
	h.CommitChange("main", ".no-mistakes.yaml", config, "configure isolated fixture commands")
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("publish trusted fixture: %v %s", err, out)
	}
	initFromOwnWorktree(t, h, "initialize-isolation")
	const branch = "feature/test-isolation"
	h.CommitChange(branch, "feature.txt", "bounded fixture change\n", "exercise isolated Test")
	operator := h.AddWorktree(branch)
	output, err := h.RunInDir(operator, "axi", "run", "--isolate-test", "--intent", "exercise the isolated Test boundary with a deterministic model fixture")
	if err != nil {
		t.Fatalf("isolated launch: %v\n%s", err, output)
	}
	if repair {
		parked := h.ActiveRun(branch)
		if parked == nil || !strings.Contains(output, "gate:") {
			t.Fatalf("expected the synthetic baseline failure gate: %+v\n%s", parked, output)
		}
		// Only this harness-owned temporary daemon is restarted. Recovery must
		// retain the opt-in and create another fresh PostgreSQL cluster.
		if out, err := h.Run("daemon", "restart", "--force"); err != nil {
			t.Fatalf("restart isolated fixture daemon: %v\n%s", err, out)
		}
		parkedStep, ok := findStep(h.RunInfo(parked.ID).Steps, types.StepTest)
		if !ok || parkedStep.FindingsJSON == nil {
			t.Fatal("synthetic baseline failure findings are missing")
		}
		findings, err := types.ParseFindingsJSON(*parkedStep.FindingsJSON)
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, finding := range findings.Items {
			if finding.Category == types.FindingCategoryTestCommand {
				ids = append(ids, finding.ID)
			}
		}
		if len(ids) != 1 {
			t.Fatalf("expected exactly one synthetic baseline failure: %+v", findings.Items)
		}
		h.RespondToFindings(parked.ID, types.StepTest, types.ActionFix, ids)
	}
	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("isolated run not completed: %+v\n%s", run, output)
	}
	test, ok := findStep(run.Steps, types.StepTest)
	if !ok || test.Status != types.StepStatusCompleted {
		t.Fatalf("Test did not complete: %+v", test)
	}
	wantRequests := int32(1)
	if repair {
		wantRequests = 3
	}
	if modelRequests.Load() != wantRequests {
		t.Fatalf("expected %d actual Test model bridge requests, got %d", wantRequests, modelRequests.Load())
	}
	if repair && (run.SubmittedHeadSHA == nil || run.HeadSHA == *run.SubmittedHeadSHA) {
		t.Fatal("isolated repair did not durably advance the parent run's head")
	}
	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	enabled, err := database.TestIsolationEnabled(run.ID)
	if err != nil || !enabled {
		t.Fatalf("run isolation was not durably pinned: %v %v", enabled, err)
	}
}

const isolatedDeckFixture = `#!/usr/bin/python3
import json,os,socket,subprocess,sys,urllib.request
if '--help' in sys.argv:
 print('deck run --no-project-context');sys.exit(0)
if '--version' in sys.argv:
 print('deck fixture');sys.exit(0)
prompt=sys.stdin.read()
if os.environ.get('NM_TEST_DATABASE_URL'):
 assert 'UNRECOGNIZED_CREDENTIAL' not in os.environ
 assert 'AWS_SECRET_ACCESS_KEY' not in os.environ
 assert 'NM_TEST_MODEL_KEY_FILE' not in os.environ
 assert 'SSH_AUTH_SOCK' not in os.environ
 assert os.environ['DATABASE_URL']==os.environ['NM_TEST_DATABASE_URL']
 pg_env=dict(os.environ,LD_LIBRARY_PATH='/nm/postgres/lib')
 proof=subprocess.check_output(['/nm/postgres/bin/psql',os.environ['DATABASE_URL'],'-X','-A','-t','-v','ON_ERROR_STOP=1','-c',"CREATE TABLE IF NOT EXISTS proof(value TEXT); BEGIN; INSERT INTO proof VALUES('real transaction'); COMMIT; SELECT value FROM proof"],env=pg_env,timeout=3)
 assert b'real transaction' in proof
 host,port='HOST_CANARY'.split(':')
 for address in [(host,int(port)),('203.0.113.1',443)]:
  try:
   connection=socket.create_connection(address,timeout=0.3)
  except OSError: pass
  else:
   connection.close();raise AssertionError('forbidden egress')
 request=urllib.request.Request(os.environ['PROXAI_BASE_URL']+'/responses',data=b'{"model":"fixture-model","stream":true}',headers={'Content-Type':'application/json'})
 assert b'fixture-model-response' in urllib.request.urlopen(request,timeout=3).read()
result={'findings':[],'summary':'fixture checks passed','risk_level':'low','risk_rationale':'deterministic fixture','risk_scope':'source-or-external','tested':['isolated fixture'],'testing_summary':'fixture checks passed','artifacts':[],'scenarios':[{'name':'isolated fixture','result':'pass','live':True,'evidence':'transaction and egress probes','reason':''}],'verdict':'go','unsupported_rules':[],'title':'test: isolated fixture','body':'fixture'}
if 'Fix the failing tests in this repository.' in prompt:
 open('repaired.txt','w').write('isolated repair fixture\n')
print(json.dumps({'type':'run_finished','output':json.dumps(result)}))
`
