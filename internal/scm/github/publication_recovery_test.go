package github

import (
	"context"
	"strings"
	"testing"
)

func TestFindPublicationPRIncludesClosedAndRequiresExactIdentity(t *testing.T) {
	const command = "gh pr list --head feature --repo test/repo --limit 1000 --state all --json number,url,baseRefName,headRefOid,headRefName,headRepositoryOwner,headRepository"
	for _, tc := range []struct {
		name, body, want string
		found            bool
	}{
		{"closed-inclusive", "[{\"number\":42,\"url\":\"https://github.com/test/repo/pull/42\",\"baseRefName\":\"main\",\"headRefOid\":\"head\",\"headRefName\":\"feature\",\"headRepository\":{\"nameWithOwner\":\"test/repo\"}}]", "", true},
		{"missing-source", `[{"number":42,"url":"https://github.com/test/repo/pull/42","baseRefName":"main","headRefOid":"head"}]`, "source repository", false},
		{"missing-head", `[{"number":42,"url":"https://github.com/test/repo/pull/42","baseRefName":"main","headRefName":"feature","headRepository":{"nameWithOwner":"test/repo"}}]`, "exact PR head", false},
		{"foreign-source", `[{"number":42,"url":"https://github.com/test/repo/pull/42","baseRefName":"main","headRefOid":"head","headRefName":"feature","headRepository":{"nameWithOwner":"someone/repo"}}]`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := New(githubTestCmdFactory(map[string]githubTestResponse{command: {stdout: tc.body}}), nil, "", "test/repo")
			pr, err := host.FindPublicationPR(context.Background(), "feature")
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if err != nil || (pr != nil) != tc.found {
				t.Fatalf("PR=%+v error=%v", pr, err)
			}
		})
	}
}
