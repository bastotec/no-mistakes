package github

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestPublicationCommandsIgnoreAmbientGHHost(t *testing.T) {
	t.Setenv("GH_HOST", "mirror.example.test")
	for _, tc := range []struct {
		name, host, repo string
	}{
		{"github.com", "github.com", "test/repo"},
		{"qualified github.com", "github.com", "github.com/test/repo"},
		{"SSH alias", "github.com", HostPrefixedSlugForHost("git@work:test/repo.git", "github.com")},
		{"enterprise SSH alias", "ghe.example.test", HostPrefixedSlugForHost("git@work:test/repo.git", "ghe.example.test")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			url := "https://" + tc.host + "/test/repo/pull/42"
			content := scm.PRContent{Title: "Recovered publication", Body: "Recorded finding dispositions and evidence"}
			repoFlag := " --repo " + tc.host + "/test/repo"
			responses := map[string]githubTestResponse{
				"gh pr list --head feature" + repoFlag + " --state open --json number,url,baseRefName":                {stdout: "[]"},
				"gh pr create --head feature --base main" + repoFlag + " --title " + content.Title + " --body-file -": {stdout: url, wantStdin: content.Body},
				"gh pr list --head feature" + repoFlag + " --limit 1000 --state all --json number,url,baseRefName,headRefOid,headRefName,headRepositoryOwner,headRepository": {
					stdout: fmt.Sprintf(`[{"number":42,"url":%q,"baseRefName":"main","headRefOid":"head","headRefName":"feature","headRepository":{"nameWithOwner":"test/repo"}}]`, url),
				},
			}
			calls := 0
			h := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
				calls++
				destination := os.Getenv("GH_HOST")
				for i, arg := range args {
					if arg == "--repo" && i+1 < len(args) && strings.Count(args[i+1], "/") == 2 {
						destination, _, _ = strings.Cut(args[i+1], "/")
					}
				}
				if destination != tc.host {
					t.Fatalf("command would route to %q before publication: %s %q", destination, name, args)
				}
				return githubTestCmdFactory(responses)(ctx, name, args...)
			}, nil, tc.host, tc.repo)
			identity := h.PublicationIdentity()
			if pr, err := h.FindPR(ctx, "feature", ""); err != nil || pr != nil {
				t.Fatalf("initial lookup: PR=%+v error=%v", pr, err)
			}
			created, err := h.CreatePR(ctx, "feature", "main", content)
			if err != nil || created.URL != url {
				t.Fatalf("creation: PR=%+v error=%v", created, err)
			}
			found, err := h.FindPublicationPR(ctx, "feature")
			if err != nil || found == nil || found.URL != url || found.HeadSHA != "head" {
				t.Fatalf("recovery lookup: PR=%+v error=%v", found, err)
			}
			if calls != 3 || h.PublicationIdentity() != identity {
				t.Fatalf("calls=%d identity=%q, want three commands and unchanged identity %q", calls, h.PublicationIdentity(), identity)
			}
		})
	}
}

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
