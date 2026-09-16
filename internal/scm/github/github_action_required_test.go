package github

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// GitHub concludes a fork PR's workflow run action_required while it waits for
// a maintainer's approval; no job ran. GetChecks must never report it as a
// failure, on the head-commit path or on gh's own bucket (which says "fail"),
// while a real failure beside it still is one.
func TestGetChecksActionRequiredIsNotAFailure(t *testing.T) {
	t.Parallel()

	commitHost := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 123 --repo test/repo --json headRefOid --jq .headRefOid": {stdout: "deadbeef\n"},
		githubCommitChecksCommand("", "test/repo", "deadbeef"):               {stdout: githubCommitChecksResponse(`[]`)},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":2,"workflow_runs":[{"id":101,"name":"CI","status":"completed","conclusion":"action_required"},{"id":102,"name":"Lint","status":"completed","conclusion":"failure"}]}]` + "\n",
		},
	}), nil, "", "test/repo")
	prChecksHost := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr checks 123 --repo test/repo --json name,state,bucket,completedAt,link": {
			stdout: `[{"name":"CI","state":"ACTION_REQUIRED","bucket":"fail"},{"name":"Lint","state":"FAILURE","bucket":"fail"}]` + "\n",
		},
	}), nil, "", "test/repo")

	for name, tc := range map[string]struct {
		host *Host
		pr   *scm.PR
	}{
		"head commit":  {commitHost, &scm.PR{Number: "123", HeadSHA: "deadbeef"}},
		"gh pr checks": {prChecksHost, &scm.PR{Number: "123"}},
	} {
		checks, err := tc.host.GetChecks(context.Background(), tc.pr)
		if err != nil {
			t.Fatalf("%s: GetChecks() error = %v", name, err)
		}
		byName := map[string]scm.Check{}
		for _, check := range checks {
			byName[check.Name] = check
		}
		held, ok := byName["CI"]
		if !ok || held.Failing() || !held.AwaitingApproval() {
			t.Fatalf("%s: action_required check = %+v (found %v), want awaiting approval and not failing", name, held, ok)
		}
		if lint := byName["Lint"]; !lint.Failing() {
			t.Fatalf("%s: failure check = %+v, want failing", name, lint)
		}
	}
}
