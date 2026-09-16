package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func exitCodeOf(err error) int {
	var exit *exitError
	if errors.As(err, &exit) {
		return exit.code
	}
	return 0
}

const axiExistingPRURL = "https://github.com/upstream/widgets/pull/168"

func TestAxiExistingPRFlagRejectsUnsupportedCombinations(t *testing.T) {
	for _, extra := range [][]string{{"--base-branch", "release"}, {"--skip", "pr"}, {"--launch-nonce", "nonce", "--validation-generation", "gen"}} {
		args := append([]string{"axi", "run", "--existing-pr", axiExistingPRURL}, extra...)
		out, err := executeCmd(args...)
		if err == nil || !strings.Contains(out, "cannot be combined") {
			t.Fatalf("args=%v error=%v output=%s", args, err, out)
		}
	}
	for _, url := range []string{"", "168", "https://other.example/upstream/widgets/pull/168"} {
		if out, err := executeCmd("axi", "run", "--existing-pr", url); err == nil {
			t.Fatalf("accepted %q: %s", url, out)
		}
	}
}

// Retiring an association starts no run, so a flag that describes a run - above
// all --intent, which describes work the caller believes is about to be
// validated - is refused rather than silently dropped. Flags that only shape
// how a run is driven are not refused: a harness that always appends them must
// still be able to retire.
func TestAxiRetireExistingPRRejectsLaunchFlags(t *testing.T) {
	for _, extra := range [][]string{
		{"--intent", "revalidate after retiring"},
		{"--skip", "lint"},
		{"--base-branch", "release"},
		{"--launch-nonce", "nonce", "--validation-generation", "gen"},
		{"--existing-pr", axiExistingPRURL},
	} {
		args := append([]string{"axi", "run", "--retire-existing-pr"}, extra...)
		out, err := executeCmd(args...)
		if err == nil || !strings.Contains(out, "cannot be combined") {
			t.Fatalf("args=%v error=%v output=%s", args, err, out)
		}
		if !strings.Contains(out, extra[0]) {
			t.Fatalf("failure did not name the discarded flag %s: %s", extra[0], out)
		}
	}
	// --yes and --wait shape how a run is driven, not what it validates, so a
	// harness that appends them to every command can still retire.
	for _, extra := range [][]string{{"--yes"}, {"--wait", "10m"}, {"--yes", "--wait", "10m"}} {
		args := append([]string{"axi", "run", "--retire-existing-pr"}, extra...)
		out, _ := executeCmd(args...)
		if strings.Contains(out, "cannot be combined") {
			t.Fatalf("args=%v refused a drive-only flag: %s", args, out)
		}
	}
}

// `runs.existing_pr_url` is absent for an ordinary run's whole life and while
// an explicit launch is still proving its target, and an unlocked lookup cannot
// tell those apart. So it adjudicates neither: once an active run is known to
// exist the call goes to the daemon, which holds the branch lock. None of the
// fresh-run gates apply - a reattach never had to pass them - so no --intent
// and an uncommitted working tree must not turn into refusals of their own.
func TestAxiExistingPRActiveRunIsAdjudicatedByTheDaemon(t *testing.T) {
	var seen atomic.Int32
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		existingPRRun: func(p ipc.StartExistingPRRunParams) (string, error) {
			seen.Add(1)
			if p.URL != axiExistingPRURL {
				return "", fmt.Errorf("unexpected target %s", p.URL)
			}
			return "run-timeout", nil
		},
	})
	fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) { return fx.running(), nil })
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) { return fx.completed(), nil })
	if err := os.WriteFile("uncommitted.txt", []byte("operator kept editing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := executeCmd("axi", "run", "--existing-pr", axiExistingPRURL, "--wait", "3s")
	if err != nil {
		t.Fatalf("reattach refused: %v\n%s", err, out)
	}
	if seen.Load() != 1 {
		t.Fatalf("daemon consulted %d times, want 1: %s", seen.Load(), out)
	}
}

// The exit code a daemon refusal reaches the caller with is a property of the
// refusal, not of whether a run happened to be active when the CLI looked, so
// the fresh-run path maps it exactly as the active-run path does.
func TestAxiExistingPRFreshRunRefusalKeepsTheSameExitCode(t *testing.T) {
	const refusal = "submitted head does not match local source branch"
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		existingPRRun: func(ipc.StartExistingPRRunParams) (string, error) { return "", fmt.Errorf(refusal) },
	})
	fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) { return nil, nil })
	out, err := executeCmd("axi", "run", "--existing-pr", axiExistingPRURL, "--intent", "validate upstream contribution", "--wait", "3s")
	if !strings.Contains(out, refusal) {
		t.Fatalf("daemon refusal not reported: %v\n%s", err, out)
	}
	if code := exitCodeOf(err); code != 2 {
		t.Fatalf("fresh-run refusal exit code = %d, want 2: %s", code, out)
	}
}

// A conflict the daemon reasoned about is something the caller can rephrase, so
// it keeps exit 2; a daemon that does not know the method is not, so it stays
// exit 1.
func TestAxiExistingPRActiveRunConflictKeepsItsExitCode(t *testing.T) {
	const conflict = "branch already has an active run; explicit PR target cannot replace it"
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		existingPRRun: func(ipc.StartExistingPRRunParams) (string, error) { return "", fmt.Errorf(conflict) },
	})
	fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) { return fx.running(), nil })
	out, err := executeCmd("axi", "run", "--existing-pr", axiExistingPRURL, "--wait", "3s")
	if !strings.Contains(out, conflict) {
		t.Fatalf("daemon refusal not reported: %v\n%s", err, out)
	}
	if code := exitCodeOf(err); code != 2 {
		t.Fatalf("active-run conflict exit code = %d, want 2: %s", code, out)
	}

	old := newAxiTimeoutFixture(t, axiTimeoutOpts{})
	old.setGetActive(func(context.Context) (*ipc.RunInfo, error) { return old.running(), nil })
	out, err = executeCmd("axi", "run", "--existing-pr", axiExistingPRURL, "--wait", "3s")
	if !strings.Contains(out, ipc.MethodStartExistingPRRun) {
		t.Fatalf("old daemon refusal not reported: %v\n%s", err, out)
	}
	if code := exitCodeOf(err); code != 1 {
		t.Fatalf("unsupported-method exit code = %d, want 1: %s", code, out)
	}
}

func TestAxiExistingPROldDaemonCannotFallBackToOrdinaryLaunch(t *testing.T) {
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{})
	fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) { return nil, nil })
	out, err := executeCmd("axi", "run", "--existing-pr", axiExistingPRURL, "--intent", "validate upstream contribution")
	if err == nil || !strings.Contains(out, ipc.MethodStartExistingPRRun) {
		t.Fatalf("expected dedicated unsupported-method refusal, got %v\n%s", err, out)
	}
}
