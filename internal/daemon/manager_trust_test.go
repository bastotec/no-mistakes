package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestConfinedLaunchRememberedPRDisablesIntegrationCallbacks(t *testing.T) {
	t.Setenv("NM_DEMO", "")
	if runtime.GOOS == "windows" {
		t.Skip("POSIX callback fixture")
	}
	bin := t.TempDir()
	cmd := exec.Command("go", "build", "-o", filepath.Join(bin, "gh"), "../pipeline/fakecli")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake gh: %v %s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "gh")
	for _, mode := range []string{"ordinary", "confined", "isolated-test"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			p := paths.WithRoot(filepath.Join(root, "state"))
			t.Setenv("NM_HOME", filepath.Join(root, "state"))
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			database, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			repo, _ := setupTestGitRepo(t, p, database, "callback-pr")
			repo, err = database.UpdateRepoForkURL(repo.ID, "https://github.com/contributor/widgets.git")
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "host-callback")
			hooks := filepath.Join(repo.WorkingPath, ".githooks")
			if err := os.Mkdir(hooks, 0755); err != nil {
				t.Fatal(err)
			}
			hook := "#!/bin/sh\nwhile read old new ref; do\ncase \"$ref\" in refs/remotes/no-mistakes-upstream/*) printf '%s\\n' \"$ref\" >> '" + marker + "';; esac\ndone\n"
			if err := os.WriteFile(filepath.Join(hooks, "reference-transaction"), []byte(hook), 0755); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, repo.WorkingPath, "add", ".")
			gitCmd(t, repo.WorkingPath, "commit", "-m", "submitted integration hook")
			head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
			gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
			gitCmd(t, p.RepoDir(repo.ID), "config", "core.hooksPath", ".githooks")
			stubExplicitUpstream(t, p, repo, "release")
			const target = "https://github.com/upstream/widgets/pull/168"
			prior, err := database.InsertRun(repo.ID, "feature", head, head)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.AssociateRunWithExistingPR(prior.ID, repo.ID, "feature", target, "release"); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(prior.ID, types.RunCompleted); err != nil {
				t.Fatal(err)
			}
			payload := fmt.Sprintf(`{"number":168,"html_url":%q,"state":"open","base":{"ref":"release","repo":{"full_name":"upstream/widgets","html_url":"https://github.com/upstream/widgets"}},"head":{"ref":"feature","sha":%q,"repo":{"full_name":"contributor/widgets","html_url":"https://github.com/contributor/widgets"}}}`, target, head)
			t.Setenv("FAKE_CLI_EXISTING_PR_JSON", payload)
			cfg := fmt.Sprintf("agent: deck\nagent_path_override:\n  deck: %q\n", filepath.Join(root, "missing-deck"))
			if err := os.WriteFile(p.ConfigFile(), []byte(cfg), 0600); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			switch mode {
			case "confined":
				ctx = context.WithValue(ctx, confinedRunLaunchKey{}, true)
			case "isolated-test":
				ctx = context.WithValue(ctx, isolatedTestLaunchKey{}, true)
			}
			mgr := NewRunManager(database, p, nil)
			_, err = mgr.startRunWithIntentSource(ctx, repo, "feature", head, head, "fixture", nil, "fixture", db.RunIntentSourceAgent, "", "", "")
			if err == nil || !strings.Contains(err.Error(), "no runnable agent found for configured agent deck") {
				t.Fatalf("setup must stop after the integration fetch and before execution: %v", err)
			}
			ref := "refs/remotes/no-mistakes-upstream/upstream/widgets/release"
			if got := gitOutput(t, p.RepoDir(repo.ID), "rev-parse", ref); got != head {
				t.Fatalf("integration fetch = %s, want %s", got, head)
			}
			data, err := os.ReadFile(marker)
			if mode == "ordinary" {
				if err != nil || !strings.Contains(string(data), ref) {
					t.Fatalf("ordinary integration hook did not run: %q %v", data, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("%s integration hook ran on host: %q %v", mode, data, err)
			}
		})
	}
}

func TestRunConfigFetchCallbackPolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX callback fixture")
	}
	for _, mode := range []string{"ordinary", "confined", "isolated-test"} {
		for _, conditional := range []bool{false, true} {
			for _, direct := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/conditional=%v/direct=%v", mode, conditional, direct), func(t *testing.T) {
					ctx := context.Background()
					root, err := filepath.EvalSymlinks(t.TempDir())
					if err != nil {
						t.Fatal(err)
					}
					write := func(path, contents string, perm os.FileMode) {
						t.Helper()
						if err := os.WriteFile(path, []byte(contents), perm); err != nil {
							t.Fatal(err)
						}
					}
					src := filepath.Join(root, "source")
					if err := os.MkdirAll(filepath.Join(src, ".githooks"), 0o755); err != nil {
						t.Fatal(err)
					}
					gitCmd(t, src, "init", "--initial-branch=main")
					gitCmd(t, src, "config", "user.email", "test@test.com")
					gitCmd(t, src, "config", "user.name", "Test")
					gitCmd(t, src, "config", "commit.gpgsign", "false")
					marker := filepath.Join(root, "host-callback")
					hook := func(stage string) string {
						return "#!/bin/sh\nprintf '" + stage + "\\n' >> '" + marker + "'\n"
					}
					write(filepath.Join(src, ".githooks", "reference-transaction"), hook("launch"), 0o755)
					write(filepath.Join(src, ".no-mistakes.yaml"), "commands:\n  lint: echo trusted\n", 0o644)
					gitCmd(t, src, "add", ".")
					gitCmd(t, src, "commit", "-m", "submitted callback")
					head := gitOutput(t, src, "rev-parse", "HEAD")
					gate := filepath.Join(root, "gate.git")
					gitCmd(t, src, "clone", "--bare", src, gate)
					if direct {
						gitCmd(t, gate, "remote", "set-url", "origin", filepath.Join(root, "unavailable"))
					}
					if conditional {
						include := filepath.Join(root, "conditional.gitconfig")
						write(include, "[core]\nhooksPath = .githooks\n", 0o644)
						global := filepath.Join(root, "global.gitconfig")
						pattern := "gitdir:" + filepath.ToSlash(gate) + "/worktrees/**"
						write(global, "[includeIf "+strconv.Quote(pattern)+"]\npath = "+strconv.Quote(filepath.ToSlash(include))+"\n", 0o644)
						t.Setenv("GIT_CONFIG_GLOBAL", global)
					} else {
						gitCmd(t, gate, "config", "core.hooksPath", ".githooks")
					}
					wt := filepath.Join(root, "worker")
					if err := git.WorktreeAddWithoutCallbacks(ctx, gate, wt, head); err != nil {
						t.Fatal(err)
					}
					if got := gitOutput(t, wt, "config", "--get", "core.hooksPath"); got != ".githooks" {
						t.Fatalf("hook configuration = %q", got)
					}
					p := paths.WithRoot(filepath.Join(root, "state"))
					if err := p.EnsureDirs(); err != nil {
						t.Fatal(err)
					}
					database, err := db.Open(p.DB())
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { database.Close() })
					repo, err := database.InsertRepo(src, src, "main")
					if err != nil {
						t.Fatal(err)
					}
					repo.URLsVerified = true
					run, err := database.InsertRun(repo.ID, "feature", head, head)
					if err != nil {
						t.Fatal(err)
					}
					switch mode {
					case "confined":
						err = database.EnableRunConfinement(run.ID, head, "fixture")
					case "isolated-test":
						err = database.EnableTestIsolation(run.ID)
					}
					if err != nil {
						t.Fatal(err)
					}
					advance := func() string {
						t.Helper()
						gitCmd(t, src, "commit", "--allow-empty", "-m", "advance trusted branch")
						return gitOutput(t, src, "rev-parse", "HEAD")
					}
					assertFetch := func(stage, wantSHA string) {
						t.Helper()
						if got := gitOutput(t, wt, "rev-parse", "refs/remotes/origin/main"); got != wantSHA {
							t.Fatalf("%s fetched %s, want %s", stage, got, wantSHA)
						}
						data, err := os.ReadFile(marker)
						if mode == "ordinary" {
							if err != nil || !strings.Contains(string(data), stage+"\n") {
								t.Fatalf("%s positive-control hook did not run: %q %v", stage, data, err)
							}
						} else if !os.IsNotExist(err) {
							t.Fatalf("%s host callback ran for %s: %q %v", stage, mode, data, err)
						}
					}
					fresh := advance()
					fetchCtx := ctx
					if mode != "ordinary" {
						fetchCtx = git.WithoutCallbacks(ctx)
					}
					if err := fetchRunDefaultBranch(fetchCtx, wt, repo); err != nil {
						t.Fatal(err)
					}
					assertFetch("launch", fresh)
					gitCmd(t, gate, "remote", "set-url", "origin", src)
					for _, stage := range []string{"recovery-1", "recovery-2"} {
						write(filepath.Join(wt, ".githooks", "reference-transaction"), hook(stage), 0o755)
						fresh = advance()
						mgr := NewRunManager(database, p, nil)
						cfg, err := mgr.loadRecoveredConfig(ctx, run, repo, wt)
						if err != nil {
							t.Fatal(err)
						}
						if cfg.TrustedConfigSHA != fresh || cfg.Commands.Lint != "echo trusted" {
							t.Fatalf("%s did not load fresh trusted config: %+v", stage, cfg)
						}
						assertFetch(stage, fresh)
					}
				})
			}
		}
	}
}

func TestLoadRecoveredConfig_BoundsFetchAndFailsClosed(t *testing.T) {
	oldTimeout := recoveredConfigFetchTimeout
	recoveredConfigFetchTimeout = 20 * time.Millisecond
	t.Cleanup(func() { recoveredConfigFetchTimeout = oldTimeout })

	fetchResult := make(chan error, 1)
	oldFetch := fetchRecoveredRemoteBranch
	fetchRecoveredRemoteBranch = func(ctx context.Context, _, _, _ string) error {
		select {
		case <-ctx.Done():
			fetchResult <- ctx.Err()
			return ctx.Err()
		case <-time.After(time.Second):
			err := errors.New("fetch context was not bounded")
			fetchResult <- err
			return err
		}
	}
	t.Cleanup(func() { fetchRecoveredRemoteBranch = oldFetch })

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, ".no-mistakes.yaml"), []byte("commands:\n  lint: echo pushed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	mgr := NewRunManager(database, p, nil)
	started := time.Now()
	// The bounded fetch times out, leaving trustedSHA empty. Under the
	// disable_project_settings security boundary a trusted-config fetch failure
	// must ABORT (not silently proceed as "not opted out"), so this now returns
	// an error rather than a config with empty commands.
	_, err = mgr.loadRecoveredConfig(context.Background(), &db.Run{ID: "run"}, &db.Repo{DefaultBranch: "main"}, workDir)
	if err == nil {
		t.Fatal("expected loadRecoveredConfig to abort on trusted-config fetch failure")
	}
	if !strings.Contains(err.Error(), "disable_project_settings") {
		t.Fatalf("abort error should name the boundary, got: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("load recovered config took %s, want under 1s", elapsed)
	}
	if err := <-fetchResult; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fetch error = %v, want deadline exceeded", err)
	}
}

// TestLoadTrustedRepoConfig_FailClosedOnFetchFailure is the regression test for
// the supply-chain RCE review item #1: when the default-branch fetch fails,
// startRun passes an empty trustedSHA, and loadTrustedRepoConfig MUST return
// nil even though a (potentially stale) origin/<default> ref is still present
// in the worktree's shared refs. Reading that stale ref would run a command
// the live default branch has already removed. EffectiveRepoConfig then forces
// empty commands, so the stale command does not run.
func TestLoadTrustedRepoConfig_FailClosedOnFetchFailure(t *testing.T) {
	ctx := context.Background()

	// Source repo whose default branch carries a "stale" lint command — the
	// kind of command a maintainer has since removed but a stale ref would
	// still serve.
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "init", "--initial-branch=main")
	gitCmd(t, src, "config", "user.email", "test@test.com")
	gitCmd(t, src, "config", "user.name", "Test")
	gitCmd(t, src, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".no-mistakes.yaml"),
		[]byte("commands:\n  lint: \"echo stale-command\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "add", ".")
	gitCmd(t, src, "commit", "-m", "stale command on default branch")

	bare := filepath.Join(t.TempDir(), "bare.git")
	gitCmd(t, "", "init", "--bare", bare)
	// The gate bare repo is its own origin so the linked worktree can fetch
	// main exactly the way startRun does.
	if err := git.AddRemote(ctx, bare, "origin", bare); err != nil {
		t.Fatalf("add origin to bare: %v", err)
	}
	gitCmd(t, src, "remote", "add", "origin", bare)
	gitCmd(t, src, "push", "origin", "HEAD:refs/heads/main")

	// Linked worktree sharing the bare repo's refs and config.
	wt := filepath.Join(t.TempDir(), "wt")
	headSHA := gitOutput(t, src, "rev-parse", "HEAD")
	if err := git.WorktreeAdd(ctx, bare, wt, headSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}

	// A previous successful fetch left origin/main present in the shared
	// refs — this is the stale ref the old code read after a fetch failure.
	if err := git.FetchRemoteBranch(ctx, wt, "origin", "main"); err != nil {
		t.Fatalf("prime origin/main: %v", err)
	}
	ok, err := git.RefExists(ctx, wt, "origin/main")
	if err != nil {
		t.Fatalf("RefExists origin/main: %v", err)
	}
	if !ok {
		t.Fatal("precondition failed: origin/main should be present (the stale ref)")
	}

	// THE REGRESSION: fetch "failed" → startRun passes an empty trustedSHA.
	// Even with origin/main present and carrying the stale command, the
	// trusted config must be nil so the stale command cannot run.
	got := loadTrustedRepoConfig(ctx, wt, "", "test-run")
	if got != nil {
		t.Fatalf("expected nil trusted config on empty SHA (fetch failure); got commands.lint=%q", got.Commands.Lint)
	}

	// And the effective config drops the pushed-branch command too — the
	// secure default, not a fallback to a stale or hostile copy.
	pushed := &config.RepoConfig{Commands: config.Commands{Lint: "echo pushed-branch-command"}}
	eff := config.EffectiveRepoConfig(pushed, got, false)
	if eff.Commands.Lint != "" {
		t.Fatalf("SECURITY REGRESSION: command would run after fetch failure: %q", eff.Commands.Lint)
	}
}

// TestLoadTrustedRepoConfig_PinnedSHAReadsFreshDefaultBranch proves the
// complementary side of review item #1: when the fetch succeeds, the trusted
// config is read at the exact resolved SHA (not the origin/<default> ref
// name), so it reflects the freshly fetched default-branch tip rather than a
// stale ref value. Advancing the default branch and re-fetching must yield the
// new command, not the old one.
func TestLoadTrustedRepoConfig_PinnedSHAReadsFreshDefaultBranch(t *testing.T) {
	ctx := context.Background()

	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "init", "--initial-branch=main")
	gitCmd(t, src, "config", "user.email", "test@test.com")
	gitCmd(t, src, "config", "user.name", "Test")
	gitCmd(t, src, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".no-mistakes.yaml"),
		[]byte("commands:\n  lint: \"echo stale-A\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "add", ".")
	gitCmd(t, src, "commit", "-m", "stale command A")
	staleSHA := gitOutput(t, src, "rev-parse", "HEAD")

	bare := filepath.Join(t.TempDir(), "bare.git")
	gitCmd(t, "", "init", "--bare", bare)
	if err := git.AddRemote(ctx, bare, "origin", bare); err != nil {
		t.Fatalf("add origin to bare: %v", err)
	}
	gitCmd(t, src, "remote", "add", "origin", bare)
	gitCmd(t, src, "push", "origin", "HEAD:refs/heads/main")

	// Advance the default branch to a fresh command and push.
	if err := os.WriteFile(filepath.Join(src, ".no-mistakes.yaml"),
		[]byte("commands:\n  lint: \"echo fresh-B\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "add", ".")
	gitCmd(t, src, "commit", "-m", "fresh command B")
	gitCmd(t, src, "push", "origin", "HEAD:refs/heads/main")
	freshSHA := gitOutput(t, src, "rev-parse", "HEAD")

	wt := filepath.Join(t.TempDir(), "wt")
	if err := git.WorktreeAdd(ctx, bare, wt, staleSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if err := git.FetchRemoteBranch(ctx, wt, "origin", "main"); err != nil {
		t.Fatalf("fetch main: %v", err)
	}
	resolved, err := git.ResolveRef(ctx, wt, "refs/remotes/origin/main")
	if err != nil {
		t.Fatalf("resolve origin/main: %v", err)
	}
	if resolved != freshSHA {
		t.Fatalf("resolved SHA %s != fresh default-branch tip %s", resolved, freshSHA)
	}

	trusted := loadTrustedRepoConfig(ctx, wt, resolved, "test-run")
	if trusted == nil {
		t.Fatal("expected trusted config at the pinned fresh SHA")
	}
	if trusted.Commands.Lint != "echo fresh-B" {
		t.Fatalf("trusted lint = %q, want fresh-B (read at pinned SHA, not stale ref)", trusted.Commands.Lint)
	}
}
