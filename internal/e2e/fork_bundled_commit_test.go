//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A fork-delivered project: the contributor clone's origin is the fork, the
// gate's origin is upstream, and the contributor's local main carries one
// commit already pushed to the fork's main plus one pushed nowhere. A branch
// cut from that main parks at rebase with a finding the contributor can read
// in their own clone: the base is named by repository and commit, never by a
// remote-tracking spelling that resolves differently there, and the two kinds
// of bundled commit are told apart.
func TestForkBundledCommitFindingJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	ctx := context.Background()
	mustGit := func(dir string, args ...string) string {
		t.Helper()
		out, err := h.runGit(ctx, dir, args...)
		if err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
		return string(bytes.TrimSpace(out))
	}
	commitOnMain := func(path, message string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(h.WorkDir, path), []byte(message+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(h.WorkDir, "add", path)
		mustGit(h.WorkDir, "commit", "-m", message)
	}

	parentURL := "https://github.com/parent-owner/no-mistakes.git"
	forkURL := "https://github.com/fork-owner/no-mistakes.git"
	branch := "feature/fork-bundled"

	forkDir := filepath.Join(filepath.Dir(h.UpstreamDir), "fork.git")
	if err := os.MkdirAll(forkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(forkDir, "init", "--bare", "--initial-branch=main")
	mustGit(h.WorkDir, "push", forkDir, "main")
	configureGitURLRewrite(t, h, parentURL, h.UpstreamDir)
	configureGitURLRewrite(t, h, forkURL, forkDir)
	mustGit(h.WorkDir, "remote", "set-url", "origin", parentURL)

	t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
	t.Setenv("FAKEAGENT_GH_LOG", filepath.Join(filepath.Dir(h.AgentLog), "gh-fork-bundled.log"))
	t.Setenv("FAKEAGENT_GH_PARENT", "parent-owner/no-mistakes")
	if out, err := h.Run("init", "--fork-url", forkURL); err != nil {
		t.Fatalf("init with fork URL: %v\n%s", err, out)
	}

	// The contributor's own clone: origin is the fork, upstream is the parent.
	mustGit(h.WorkDir, "remote", "set-url", "origin", forkURL)
	mustGit(h.WorkDir, "remote", "add", "upstream", parentURL)
	mustGit(h.WorkDir, "fetch", "upstream")
	commitOnMain("fork_only.txt", "fork-only merged PR")
	mustGit(h.WorkDir, "push", "origin", "main")
	mustGit(h.WorkDir, "fetch", "origin")
	commitOnMain("local_only.txt", "local-only work")
	h.CommitChange(branch, "my_fix.txt", "fix\n", "my fix")
	h.PushToGate(branch)

	run := waitForStepStatus(t, h, branch, types.StepRebase, types.StepStatusAwaitingApproval, 60*time.Second)
	rebaseStep, ok := findStep(run.Steps, types.StepRebase)
	if !ok || rebaseStep.FindingsJSON == nil {
		t.Fatalf("rebase step parked without findings: %+v", rebaseStep)
	}
	findings, err := types.ParseFindingsJSON(*rebaseStep.FindingsJSON)
	if err != nil {
		t.Fatalf("parse rebase findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("findings = %+v, want one bundled-commit finding", findings.Items)
	}
	item := findings.Items[0]
	t.Logf("summary: %s\ndescription:\n%s", findings.Summary, item.Description)

	if item.Action != types.ActionAskUser {
		t.Fatalf("finding action = %q, want ask-user", item.Action)
	}
	for _, remote := range strings.Fields(mustGit(h.WorkDir, "remote")) {
		if spelling := remote + "/main"; strings.Contains(item.Description, spelling) || strings.Contains(findings.Summary, spelling) {
			t.Fatalf("finding names %q, which the contributor clone resolves to a different commit:\n%s", spelling, item.Description)
		}
	}
	// The harness reaches both GitHub URLs through insteadOf rewrites, so git
	// reports each repository by its rewritten local URL.
	upstreamShort := mustGit(h.UpstreamDir, "rev-parse", "--short", "main")
	wantBase := "PR base " + pathFileURL(t, h.UpstreamDir) + " main at " + upstreamShort
	if !strings.Contains(item.Description, wantBase) {
		t.Fatalf("finding does not name the PR base as %q:\n%s", wantBase, item.Description)
	}
	if got, want := mustGit(h.WorkDir, "rev-parse", upstreamShort), mustGit(h.UpstreamDir, "rev-parse", "main"); got != want {
		t.Fatalf("named base %s resolves to %s in the contributor clone, want %s", upstreamShort, got, want)
	}

	pushedPart, unpushedPart, found := strings.Cut(item.Description, "\n\n")
	if !found {
		t.Fatalf("finding does not separate pushed from never-pushed commits:\n%s", item.Description)
	}
	if !strings.Contains(pushedPart, "pushed to "+pathFileURL(t, forkDir)+" main") || !strings.Contains(pushedPart, "would bring them along") ||
		!strings.Contains(pushedPart, "fork-only merged PR") || strings.Contains(pushedPart, "local-only work") {
		t.Fatalf("pushed section is wrong:\n%s", pushedPart)
	}
	if !strings.Contains(unpushedPart, "never pushed") || !strings.Contains(unpushedPart, "local-only work") ||
		strings.Contains(unpushedPart, "fork-only merged PR") {
		t.Fatalf("never-pushed section is wrong:\n%s", unpushedPart)
	}
	if !strings.Contains(findings.Summary, "absent from the PR base (1 never pushed)") {
		t.Fatalf("summary = %q", findings.Summary)
	}

	h.Respond(run.ID, types.StepRebase, types.ActionAbort)
	if completed := h.WaitForRun(branch, 60*time.Second); completed.Status != types.RunFailed {
		t.Fatalf("run status after abort = %s, want failed", completed.Status)
	}
}
