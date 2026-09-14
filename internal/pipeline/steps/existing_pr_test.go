package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

const fixtureExistingPR = "https://github.com/upstream/widgets/pull/168"
const fixtureSourceURL = "https://github.com/contributor/widgets.git"

func existingPRFixture(head string) string {
	return fmt.Sprintf(`{"number":168,"html_url":%q,"state":"open","merged":false,"base":{"ref":"main","repo":{"full_name":"upstream/widgets","html_url":"https://github.com/upstream/widgets"}},"head":{"ref":"feature","sha":%q,"repo":{"full_name":"contributor/widgets","html_url":"https://github.com/contributor/widgets"}}}`, fixtureExistingPR, head)
}

func pinFixturePR(t *testing.T, sctx *pipeline.StepContext) {
	t.Helper()
	sctx.Repo.UpstreamURL = fixtureSourceURL
	sctx.Repo.URLsVerified = true
	run, err := sctx.DB.InsertRunWithIntentAndLaunchNonce(sctx.Repo.ID, sctx.Run.Branch, sctx.Run.HeadSHA, sctx.Run.BaseSHA, nil, "", "", "", "", fixtureExistingPR)
	if err != nil {
		t.Fatal(err)
	}
	sctx.Run = run
}

// Representative reproduction of the symptom without touching a live forge.
// With no explicit target, the upstream PR is outside the fork-scoped query;
// ordinary behavior still creates on the registered fork repository.
func TestPRStep_ForkOriginWithoutExplicitTargetRetainsCreation(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sctx.Repo.UpstreamURL = fixtureSourceURL
	created := "https://github.com/contributor/widgets/pull/2"
	env, log := fakeGH(t, "")
	sctx.Env = append(env, "FAKE_CLI_CREATED_PR_URL="+created)
	out, err := (&PRStep{}).Execute(sctx)
	if err != nil || out.PRURL != created {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "pr list --head feature --repo contributor/widgets") || !strings.Contains(string(data), "pr create --head feature --base main --repo contributor/widgets") {
		t.Fatalf("unexpected discovery boundary:\n%s", data)
	}
}

func TestPRStep_ExplicitUpstreamNeverDiscoversAlternate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, replace, with string
		unavailable         bool
	}{
		{name: "valid fork origin"},
		{name: "wrong source ref", replace: `"ref":"feature"`, with: `"ref":"other"`},
		{name: "wrong source repository", replace: `"full_name":"contributor/widgets"`, with: `"full_name":"contributor/other"`},
		{name: "wrong target repository", replace: `"full_name":"upstream/widgets"`, with: `"full_name":"elsewhere/widgets"`},
		{name: "wrong head", replace: `"sha":`, with: `"ignored_sha":`},
		{name: "closed target", replace: `"state":"open"`, with: `"state":"closed"`},
		{name: "unavailable validation", unavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			pinFixturePR(t, sctx)
			env, log := fakeGH(t, "https://github.com/contributor/widgets/pull/2")
			payload := existingPRFixture(head)
			if tc.replace != "" {
				payload = strings.Replace(payload, tc.replace, tc.with, 1)
			}
			sctx.Env = append(env, "FAKE_CLI_EXISTING_PR_JSON="+payload, "FAKE_CLI_EXISTING_PR_ENDPOINT=repos/upstream/widgets/pulls/168")
			if tc.unavailable {
				sctx.Env = append(sctx.Env, "FAKE_CLI_EXISTING_PR_ERROR=unavailable")
			}
			out, err := (&PRStep{}).Execute(sctx)
			valid := tc.replace == "" && !tc.unavailable
			if valid && (err != nil || out.PRURL != fixtureExistingPR) {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			if !valid && err == nil {
				t.Fatalf("wanted failure, got %+v", out)
			}
			data, _ := os.ReadFile(log)
			calls := string(data)
			if strings.Contains(calls, "pr list") || strings.Contains(calls, "pr create") {
				t.Fatalf("explicit target fell through to discovery/create:\n%s", calls)
			}
			if !valid && strings.Contains(calls, "pr edit") {
				t.Fatalf("invalid target mutated PR:\n%s", calls)
			}
			if valid && (!strings.Contains(calls, "pr edit 168 --repo upstream/widgets") || strings.Contains(calls, "--repo contributor/widgets")) {
				t.Fatalf("wrong update routing:\n%s", calls)
			}
			stored, e := sctx.DB.GetRun(sctx.Run.ID)
			if e != nil || stored.ExistingPRURL == nil || *stored.ExistingPRURL != fixtureExistingPR {
				t.Fatalf("lost durable target: %+v %v", stored, e)
			}
		})
	}
}

func TestCIStep_ExplicitPRUsesPublishedHeadAndUpstreamHost(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	pinFixturePR(t, sctx)
	if err := sctx.DB.UpdateRunPublication(sctx.Run.ID, db.PushBinding{HeadSHA: head, TargetKind: "upstream", TargetFingerprint: "fixture", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "gh.log")
	sctx.Env = append(fakeCIGH(t, "MERGED", `[]`), "FAKE_CLI_EXISTING_PR_JSON="+existingPRFixture(head), "FAKE_CLI_LOG="+log)
	out, err := (&CIStep{}).Execute(sctx)
	if err != nil || out.Skipped {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "pr view 168 --repo upstream/widgets") || strings.Contains(string(data), "--repo contributor/widgets") {
		t.Fatalf("wrong CI routing:\n%s", data)
	}
	sctx.Env = append(sctx.Env, "FAKE_CLI_EXISTING_PR_ERROR=unavailable")
	if out, err := (&CIStep{}).Execute(sctx); err == nil {
		t.Fatalf("unavailable validation skipped or passed: %+v", out)
	}
}

func TestPushStep_ExplicitPRValidatesRemoteHeadBeforePublishing(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprintf("invalid=%v", invalid), func(t *testing.T) {
			remote := t.TempDir()
			gitCmd(t, remote, "init", "--bare")
			dir, base, prior := setupGitRepo(t)
			gitCmd(t, dir, "remote", "add", "origin", remote)
			gitCmd(t, dir, "push", "origin", "main", "feature")
			if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("pipeline fix"), 0600); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", ".")
			gitCmd(t, dir, "commit", "-m", "pipeline fix")
			next := gitCmd(t, dir, "rev-parse", "HEAD")
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, next, config.Commands{})
			pinFixturePR(t, sctx)
			// Real local Git transport; the logical GitHub URL is never contacted.
			gitCmd(t, dir, "config", "url."+remote+".insteadOf", fixtureSourceURL)
			setupGateMirror(t, sctx)
			recordReviewApproval(t, sctx, next)
			log := filepath.Join(t.TempDir(), "gh.log")
			expected := prior
			if invalid {
				expected = next
			}
			sctx.Env = append(fakeCIGH(t, "OPEN", `[]`), "FAKE_CLI_EXISTING_PR_JSON="+existingPRFixture(expected), "FAKE_CLI_PR_BODY="+compliantPipelineBody(t, prior), "FAKE_CLI_LOG="+log)
			_, err := (&PushStep{}).Execute(sctx)
			if (err != nil) != invalid {
				t.Fatalf("push err=%v", err)
			}
			want := next
			if invalid {
				want = prior
			}
			if got := gitCmd(t, remote, "rev-parse", "refs/heads/feature"); got != want {
				t.Fatalf("remote moved incorrectly: %s want %s", got, want)
			}
			data, _ := os.ReadFile(log)
			if strings.Contains(string(data), "pr list") || strings.Contains(string(data), "pr create") || invalid && strings.Contains(string(data), "pr edit") {
				t.Fatalf("unsafe PR calls:\n%s", data)
			}
			if !invalid && !strings.Contains(string(data), "pr edit 168 --repo upstream/widgets") {
				t.Fatalf("missing upstream attestation:\n%s", data)
			}
		})
	}
}
