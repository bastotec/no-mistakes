package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

const publishActionDir = ".github/actions/require-no-mistakes-publish"

// The server is a persisted forge, not a stubbed verdict. Each subprocess runs
// the actual publisher and unchanged verifier; only GitHub HTTP I/O is local.
type publishForge struct {
	mu         sync.Mutex
	pr         map[string]any
	check      map[string]any
	writes     []map[string]any
	getFailure bool
	attempts   map[int]int
	oldEntered chan struct{}
	releaseOld chan struct{}
}

func publishPR(body, head string) map[string]any {
	return map[string]any{
		"number": 31, "body": body,
		"head": map[string]any{"sha": head, "ref": "feature"},
		"user": map[string]any{"login": "contributor"},
		"base": map[string]any{"repo": map[string]any{"full_name": "owner/repo"}},
	}
}

func publishBody(head string) string {
	return "Updates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)\n" +
		fmt.Sprintf(`<!-- no-mistakes-pipeline-attestation:v1 {"head_sha":%q,"steps":[{"step":"review","status":"completed"},{"step":"test","status":"completed"},{"step":"document","status":"completed"}]} -->`, head)
}

func (f *publishForge) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/repos/owner/repo/")
	if path == "actions/runs/90" && f.oldEntered != nil {
		close(f.oldEntered)
		<-f.releaseOld
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.HasPrefix(path, "actions/runs/") {
		var number int
		fmt.Sscanf(path, "actions/runs/%d", &number)
		attempt := f.attempts[number]
		if attempt == 0 {
			attempt = 1
		}
		json.NewEncoder(w).Encode(map[string]any{
			"workflow_id": 7, "run_number": number, "run_attempt": attempt,
			"event": "pull_request_target", "html_url": fmt.Sprintf("https://github.com/owner/repo/actions/runs/%d", number),
		})
		return
	}
	if r.Method == http.MethodGet && f.getFailure {
		http.Error(w, "unavailable", http.StatusForbidden)
		return
	}
	if path == "pulls/31" {
		json.NewEncoder(w).Encode(f.pr)
		return
	}
	if strings.HasPrefix(path, "commits/") {
		checks := []map[string]any{}
		if f.check != nil {
			checks = append(checks, f.check)
		}
		json.NewEncoder(w).Encode(map[string]any{"check_runs": checks})
		return
	}
	if (path == "check-runs" && r.Method == http.MethodPost) || (path == "check-runs/100" && r.Method == http.MethodPatch) {
		var data map[string]any
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			http.Error(w, "bad body", 400)
			return
		}
		output, _ := data["output"].(map[string]any)
		summary, _ := output["summary"].(string)
		if len(summary) > 65535 {
			http.Error(w, "summary exceeds Checks API limit", http.StatusUnprocessableEntity)
			return
		}
		f.writes = append(f.writes, data)
		data["id"] = 100
		data["app"] = map[string]any{"slug": "github-actions"}
		f.check = data
		json.NewEncoder(w).Encode(data)
		return
	}
	http.Error(w, "unexpected request "+r.Method+" "+path, 404)
}

type publisherResult struct {
	output string
	err    error
}

func startPublisher(t *testing.T, api string, number int, pr map[string]any) <-chan publisherResult {
	t.Helper()
	return startPublisherAttempt(t, api, number, 1, pr)
}

func startPublisherAttempt(t *testing.T, api string, number, attempt int, pr map[string]any) <-chan publisherResult {
	t.Helper()
	dir := t.TempDir()
	event, _ := json.Marshal(map[string]any{"number": 31, "pull_request": pr})
	path := filepath.Join(dir, "event.json")
	if err := os.WriteFile(path, event, 0600); err != nil {
		t.Fatal(err)
	}
	action := loadRequireAction(t, publishActionDir)
	if len(action.Runs.Steps) != 1 {
		t.Fatalf("publisher action steps = %d, want 1", len(action.Runs.Steps))
	}
	actionPath, err := filepath.Abs(publishActionDir)
	if err != nil {
		t.Fatal(err)
	}
	pythonInterpreter(t)
	cmd := exec.Command(action.Runs.Steps[0].Shell, "-c", action.Runs.Steps[0].Run)
	// Do not inherit caller inputs, exemptions, tokens or an Actions output path.
	for _, item := range os.Environ() {
		key := strings.SplitN(item, "=", 2)[0]
		if strings.HasPrefix(key, "GITHUB_") || strings.HasPrefix(key, "PR_") || strings.HasPrefix(key, "NM_EXEMPT_") {
			continue
		}
		cmd.Env = append(cmd.Env, item)
	}
	cmd.Env = append(cmd.Env,
		"PYTHONDONTWRITEBYTECODE=1", "GITHUB_ACTION_PATH="+actionPath,
		"GITHUB_TOKEN=test-token", "GITHUB_REPOSITORY=owner/repo",
		"GITHUB_API_URL="+api, "GITHUB_EVENT_NAME=pull_request_target", "GITHUB_EVENT_PATH="+path,
		fmt.Sprintf("GITHUB_RUN_ID=%d", number), fmt.Sprintf("GITHUB_RUN_NUMBER=%d", number),
		fmt.Sprintf("GITHUB_RUN_ATTEMPT=%d", attempt), "GITHUB_OUTPUT="+filepath.Join(dir, "outputs"),
		"GITHUB_STEP_SUMMARY="+filepath.Join(dir, "summary"))
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	result := make(chan publisherResult, 1)
	go func() { err := cmd.Wait(); result <- publisherResult{output.String(), err} }()
	return result
}

func requirePublication(t *testing.T, ch <-chan publisherResult, verdict, publication string) {
	t.Helper()
	result := <-ch
	if result.err != nil {
		t.Fatalf("publisher: %v\n%s", result.err, result.output)
	}
	for _, want := range []string{"Original event verdict: " + verdict, "Current verdict publication: " + publication} {
		if !strings.Contains(result.output, want) {
			t.Fatalf("missing %q:\n%s", want, result.output)
		}
	}
}

func TestPublishVerdict_DelayedSynchronizeCannotSupersedeEditedSuccess(t *testing.T) {
	head := strings.Repeat("8", 40)
	old := publishPR(publishBody(strings.Repeat("3", 40)), head)
	current := publishPR(publishBody(head), head)
	forge := &publishForge{pr: current, oldEntered: make(chan struct{}), releaseOld: make(chan struct{})}
	server := httptest.NewServer(http.HandlerFunc(forge.serve))
	defer server.Close()
	// Hold the obsolete queued event before it can consult publication state.
	older := startPublisher(t, server.URL, 90, old)
	<-forge.oldEntered
	requirePublication(t, startPublisher(t, server.URL, 93, current), "success", "published")
	close(forge.releaseOld)
	requirePublication(t, older, "failure", "obsolete")
	if len(forge.writes) != 1 || forge.check["conclusion"] != "success" || forge.check["external_id"] != "no-mistakes-current-v1:31:7:93:1" {
		t.Fatalf("obsolete event changed current verdict: %#v", forge.writes)
	}
}

func TestPublishVerdict_InOrderFailureThenEditedSuccess(t *testing.T) {
	head := strings.Repeat("8", 40)
	old := publishPR(publishBody(strings.Repeat("3", 40)), head)
	forge := &publishForge{pr: old}
	server := httptest.NewServer(http.HandlerFunc(forge.serve))
	defer server.Close()
	requirePublication(t, startPublisher(t, server.URL, 90, old), "failure", "published")
	if forge.check["conclusion"] != "failure" {
		t.Fatal("head mismatch must remain red")
	}
	current := publishPR(publishBody(head), head)
	forge.mu.Lock()
	forge.pr = current
	forge.mu.Unlock()
	requirePublication(t, startPublisher(t, server.URL, 93, current), "success", "published")
	if len(forge.writes) != 2 || forge.check["conclusion"] != "success" {
		t.Fatalf("in-order writes: %#v", forge.writes)
	}
	if _, recreated := forge.writes[1]["head_sha"]; recreated {
		t.Fatal("must update the same check, not create another")
	}
}

func TestPublishVerdict_BoundsFailureSummaryWithoutDroppingRedVerdict(t *testing.T) {
	head := strings.Repeat("8", 40)
	attestedHead := strings.Repeat("x", 65250)
	body := publishBody(attestedHead)
	if len(body) > 65536 {
		t.Fatalf("test PR body exceeds GitHub limit: %d", len(body))
	}
	current := publishPR(body, head)
	forge := &publishForge{pr: current}
	server := httptest.NewServer(http.HandlerFunc(forge.serve))
	defer server.Close()

	result := <-startPublisher(t, server.URL, 93, current)
	if result.err != nil {
		t.Fatalf("publisher: %v\n%s", result.err, result.output)
	}
	if !strings.Contains(result.output, attestedHead) {
		t.Fatal("full event evidence was not retained in workflow output")
	}
	if len(forge.writes) != 1 || forge.check["conclusion"] != "failure" {
		t.Fatalf("oversized failure did not publish red verdict: %#v", forge.writes)
	}
	output := forge.check["output"].(map[string]any)
	summary := output["summary"].(string)
	if len(summary) != 65535 || !strings.HasSuffix(summary, "[Event evidence truncated; full evidence remains in workflow logs.]") {
		t.Fatalf("check summary was not safely truncated: length=%d", len(summary))
	}
}

func TestPublishVerdict_AuthorityAndRerunBoundaries(t *testing.T) {
	head := strings.Repeat("8", 40)
	good := publishBody(head)
	for _, tc := range []struct {
		name, eventBody, liveBody, liveHead, watermark, wantVerdict, wantPublication string
		apiFail                                                                      bool
		wantRed                                                                      bool
	}{
		{"same_head_body_edit", "invalid", good, head, "", "failure", "obsolete", false, false},
		{"changed_head", good, good, strings.Repeat("9", 40), "", "success", "obsolete", false, false},
		{"current_head_mismatch", publishBody(strings.Repeat("3", 40)), publishBody(strings.Repeat("3", 40)), head, "", "failure", "published", false, true},
		{"current_body_invalid", "invalid", "invalid", head, "", "failure", "published", false, true},
		{"old_event_restored_body", good, good, head, "no-mistakes-current-v1:31:7:94:1", "success", "obsolete", false, false},
		{"old_rerun", good, good, head, "no-mistakes-current-v1:31:7:94:2", "success", "obsolete", false, false},
		{"idempotent_write", good, good, head, "no-mistakes-current-v1:31:7:93:1", "success", "unchanged", false, false},
		{"later_attempt_already_published", good, good, head, "no-mistakes-current-v1:31:7:93:2", "success", "obsolete", false, false},
		{"different_workflow", good, good, head, "no-mistakes-current-v1:31:8:93:1", "success", "", false, false},
		{"malformed_watermark", good, good, head, "no-mistakes-current-v1:31:broken", "success", "", false, false},
		{"api_unavailable", good, good, head, "", "success", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forge := &publishForge{pr: publishPR(tc.liveBody, tc.liveHead), getFailure: tc.apiFail}
			if tc.watermark != "" {
				forge.check = map[string]any{"id": 100, "name": "PR must be raised via no-mistakes", "external_id": tc.watermark, "conclusion": "success", "app": map[string]any{"slug": "github-actions"}}
			}
			server := httptest.NewServer(http.HandlerFunc(forge.serve))
			defer server.Close()
			result := startPublisher(t, server.URL, 93, publishPR(tc.eventBody, head))
			if tc.wantPublication == "" {
				r := <-result
				if r.err == nil {
					t.Fatalf("authority failure passed: %s", r.output)
				}
			} else {
				requirePublication(t, result, tc.wantVerdict, tc.wantPublication)
			}
			if tc.wantRed {
				if len(forge.writes) != 1 || forge.check["conclusion"] != "failure" {
					t.Fatalf("current failure not red: %#v", forge.writes)
				}
			} else if len(forge.writes) != 0 {
				t.Fatalf("unauthorized writes: %#v", forge.writes)
			}
		})
	}
}

func TestPublishVerdict_CallerIsTrustedAndSerializesWholeJob(t *testing.T) {
	data, err := os.ReadFile(publishActionDir + "/caller.yml.example")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On          map[string]any    `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			Name        string `yaml:"name"`
			Concurrency struct {
				Group  string `yaml:"group"`
				Cancel bool   `yaml:"cancel-in-progress"`
			} `yaml:"concurrency"`
			Steps []struct {
				Uses string `yaml:"uses"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if len(workflow.On) != 1 || workflow.On["pull_request_target"] == nil {
		t.Fatal("requires trusted target trigger")
	}
	if workflow.Permissions["checks"] != "write" || workflow.Permissions["pull-requests"] != "read" || workflow.Permissions["actions"] != "read" {
		t.Fatal("publisher permissions missing")
	}
	job := workflow.Jobs["publish"]
	if job.Name == "PR must be raised via no-mistakes" {
		t.Fatal("runner job must not own compliance verdict")
	}
	if job.Concurrency.Group != "no-mistakes-current-${{ github.repository }}-${{ github.event.pull_request.number }}" || job.Concurrency.Cancel {
		t.Fatal("entire publisher job requires shared non-cancelling PR lock")
	}
	if len(job.Steps) != 1 || job.Steps[0].Run != "" || job.Steps[0].Uses != "bastotec/no-mistakes/.github/actions/require-no-mistakes-publish@<published-commit-sha>" {
		t.Fatal("trusted caller must run only immutable published action, no checkout or PR code")
	}
	action := loadRequireAction(t, publishActionDir)
	for _, forbidden := range []string{"pr-body", "pr-head-sha"} {
		if _, ok := action.Inputs[forbidden]; ok {
			t.Fatal("cannot override event snapshot")
		}
	}
}

func TestPublishVerdict_RerunRetainsEventRankAndTruth(t *testing.T) {
	head := strings.Repeat("8", 40)
	current := publishPR(publishBody(head), head)
	old := publishPR(publishBody(strings.Repeat("3", 40)), head)
	forge := &publishForge{pr: current, attempts: map[int]int{90: 2}}
	server := httptest.NewServer(http.HandlerFunc(forge.serve))
	defer server.Close()
	requirePublication(t, startPublisher(t, server.URL, 93, current), "success", "published")
	requirePublication(t, startPublisherAttempt(t, server.URL, 90, 2, old), "failure", "obsolete")
	if len(forge.writes) != 1 || forge.check["conclusion"] != "success" {
		t.Fatalf("old rerun superseded current success: %#v", forge.writes)
	}
	forge.mu.Lock()
	forge.attempts[93] = 2
	forge.mu.Unlock()
	requirePublication(t, startPublisherAttempt(t, server.URL, 93, 2, current), "success", "published")
	if len(forge.writes) != 2 || forge.check["external_id"] != "no-mistakes-current-v1:31:7:93:2" {
		t.Fatalf("current rerun did not advance its attempt: %#v", forge.writes)
	}
}
