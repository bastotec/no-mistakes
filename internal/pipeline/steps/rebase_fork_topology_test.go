package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// forkTopology is a fork-delivered project: the contributor clone's origin is
// the fork, the gate's origin is upstream, and the fork's main carries a
// commit (its own merged PR) that upstream's main does not have. A feature
// branch cut from local main therefore carries that commit into an upstream PR.
type forkTopology struct {
	upstream, fork   string // bare remotes
	working, gate    string // contributor clone and gate worktree
	base, head       string
	upstreamMain     string
	forkOnlySubject  string
	forkOnlyFileName string
}

func newForkTopology(t *testing.T, gateFromFork bool) forkTopology {
	t.Helper()
	var ft forkTopology
	ft.upstream = t.TempDir()
	gitCmd(t, ft.upstream, "init", "--bare", "--initial-branch=main")
	ft.fork = t.TempDir()
	gitCmd(t, ft.fork, "init", "--bare", "--initial-branch=main")

	seed := t.TempDir()
	gitCmd(t, seed, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "-A")
	gitCmd(t, seed, "commit", "-m", "base")
	ft.base = gitCmd(t, seed, "rev-parse", "HEAD")
	gitCmd(t, seed, "push", ft.upstream, "main")
	gitCmd(t, seed, "push", ft.fork, "main")

	// The fork's main gains its own merged PR, which upstream never took.
	ft.forkOnlySubject = "fork-only merged PR (#1)"
	ft.forkOnlyFileName = "fork_only.txt"
	if err := os.WriteFile(filepath.Join(seed, ft.forkOnlyFileName), []byte("fork"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "-A")
	gitCmd(t, seed, "commit", "-m", ft.forkOnlySubject)
	gitCmd(t, seed, "push", ft.fork, "main")

	// Contributor clone: origin = fork, upstream = upstream, local main = fork main.
	ft.working = t.TempDir()
	gitCmd(t, ft.working, "clone", ft.fork, ".")
	gitCmd(t, ft.working, "remote", "add", "upstream", ft.upstream)
	gitCmd(t, ft.working, "fetch", "upstream")
	gitCmd(t, ft.working, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(ft.working, "my_fix.txt"), []byte("fix"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, ft.working, "add", "-A")
	gitCmd(t, ft.working, "commit", "-m", "my fix")
	ft.head = gitCmd(t, ft.working, "rev-parse", "HEAD")
	gitCmd(t, ft.working, "checkout", "main")

	ft.gate = t.TempDir()
	gateOrigin := ft.upstream
	if gateFromFork {
		gateOrigin = ft.fork
	}
	gitCmd(t, ft.gate, "clone", gateOrigin, ".")
	gitCmd(t, ft.gate, "fetch", ft.working, "feature")
	gitCmd(t, ft.gate, "checkout", "-B", "feature", ft.head)
	ft.upstreamMain = gitCmd(t, ft.upstream, "rev-parse", "main")
	return ft
}

func (ft forkTopology) run(t *testing.T, upstreamURL, forkURL string, verified bool) *types.Findings {
	t.Helper()
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, ft.gate, ft.base, ft.head, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstreamURL
	sctx.Repo.ForkURL = forkURL
	sctx.Repo.WorkingPath = ft.working
	sctx.Repo.URLsVerified = verified
	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !outcome.NeedsApproval || outcome.Findings == "" {
		return nil
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	return &findings
}

// The bundled-commit warning compares against the PR's integration base
// (upstream, where the GitHub host opens the PR) whatever the push target is,
// and only a gate whose origin is the fork compares against the fork. These
// counterfactuals pin that the comparison follows the integration base, not
// the delivery remote.
func TestRebaseStep_BundledCommitComparisonFollowsIntegrationBase(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		record   func(forkTopology) (upstreamURL, forkURL string, verified bool)
		fromFork bool
		wantFire bool
	}{
		{"fork routing, gate origin upstream", func(ft forkTopology) (string, string, bool) { return ft.upstream, ft.fork, false }, false, true},
		{"push target is upstream, gate origin upstream", func(ft forkTopology) (string, string, bool) { return ft.upstream, "", false }, false, true},
		{"gate origin is the fork and the record agrees", func(ft forkTopology) (string, string, bool) { return ft.fork, "", false }, true, false},
		{"gate origin is the fork, record names upstream unverified", func(ft forkTopology) (string, string, bool) { return ft.upstream, ft.fork, false }, true, false},
		{"gate origin is the fork, verified record names upstream", func(ft forkTopology) (string, string, bool) { return ft.upstream, ft.fork, true }, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ft := newForkTopology(t, tc.fromFork)
			upstreamURL, forkURL, verified := tc.record(ft)
			findings := ft.run(t, upstreamURL, forkURL, verified)
			if got := findings != nil; got != tc.wantFire {
				t.Fatalf("warning fired = %v, want %v (findings=%#v)", got, tc.wantFire, findings)
			}
			if findings != nil && !strings.Contains(findings.Items[0].Description, ft.forkOnlySubject) {
				t.Fatalf("finding does not name the fork-only commit: %s", findings.Items[0].Description)
			}
		})
	}
}

// The reader resolves every ref the finding names in their own clone, where
// origin is the fork, while the gate compared against upstream under the same
// spelling. The finding must name the base by an identity that resolves to the
// same commit in both repos, and say the commits were pushed (to the fork) but
// are absent from the PR base rather than claiming they were never pushed.
func TestRebaseStep_ForkBundledCommitFindingNamesBaseUnambiguously(t *testing.T) {
	t.Parallel()
	ft := newForkTopology(t, false)
	findings := ft.run(t, ft.upstream, ft.fork, false)
	if findings == nil || len(findings.Items) != 1 {
		t.Fatalf("expected one bundled-commit finding, got %#v", findings)
	}
	item := findings.Items[0]
	desc := item.Description
	for _, repo := range []string{ft.working, ft.gate} {
		for _, remote := range strings.Fields(gitCmd(t, repo, "remote")) {
			if spelling := remote + "/main"; strings.Contains(desc, spelling) || strings.Contains(findings.Summary, spelling) {
				t.Fatalf("finding names %q, which resolves differently in the contributor clone and the gate:\n%s", spelling, desc)
			}
		}
	}
	short := ft.upstreamMain[:7]
	if !strings.Contains(desc, short) {
		t.Fatalf("finding does not name the compared base commit %s:\n%s", short, desc)
	}
	if got := gitCmd(t, ft.working, "rev-parse", short); got != gitCmd(t, ft.gate, "rev-parse", short) {
		t.Fatalf("base commit %s resolves differently: %s vs gate", short, got)
	}
	if strings.Contains(desc, "never pushed") || strings.Contains(findings.Summary, "unpushed") {
		t.Fatalf("fork-pushed commits are described as never pushed:\n%s\n%s", desc, findings.Summary)
	}
	if !strings.Contains(desc, "would bring them along") || !strings.Contains(desc, ft.forkOnlySubject) {
		t.Fatalf("finding does not say the PR would bring the fork's commits along:\n%s", desc)
	}
	if item.Action != types.ActionAskUser {
		t.Fatalf("finding action = %q, want %q", item.Action, types.ActionAskUser)
	}
}
