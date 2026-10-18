package testisolation

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
)

func isolationGit(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	binary, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func isolationGitFixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux isolation only")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(global, []byte("[user]\nname = Global Repair Author\nemail = repair@example.invalid\n[credential]\nhelper = secret-helper\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	for _, key := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	isolationGit(t, source, nil, "init")
	if err := os.WriteFile(filepath.Join(source, "tracked"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	isolationGit(t, source, nil, "add", "tracked")
	isolationGit(t, source, nil, "commit", "-m", "base")
	base := isolationGit(t, source, nil, "rev-parse", "HEAD")
	isolationGit(t, source, nil, "branch", "foreign")
	work := filepath.Join(root, "work")
	isolationGit(t, source, nil, "worktree", "add", "--detach", work, base)
	return root, source, work, base
}

func TestIsolatedGitImportsOnlyRunHeadAndResolvedIdentity(t *testing.T) {
	root, source, work, base := isolationGitFixture(t)
	workspace, err := PrepareGit(context.Background(), work, root)
	if err != nil {
		t.Fatal(err)
	}
	env := append(Environment(work), workspace.Identity...)
	run := func(args ...string) string {
		return isolationGit(t, work, env, append([]string{"--git-dir=" + workspace.Dir}, args...)...)
	}
	config := run("config", "--local", "--list")
	if strings.Contains(config, "secret-helper") || strings.Contains(config, "remote.") {
		t.Fatalf("ambient config imported: %s", config)
	}
	if err := os.WriteFile(filepath.Join(work, "tracked"), []byte("repaired\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "tracked")
	run("commit", "-m", "isolated repair")
	head := run("rev-parse", "HEAD")
	identity := run("show", "-s", "--format=%an <%ae>|%cn <%ce>", head)
	if identity != "Global Repair Author <repair@example.invalid>|Global Repair Author <repair@example.invalid>" {
		t.Fatalf("resolved identity lost: %s", identity)
	}
	marker := filepath.Join(work, "escaped")
	callback := filepath.Join(work, "callback")
	if err := os.WriteFile(callback, []byte("#!/bin/sh\necho escaped > "+marker+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	run("config", "core.fsmonitor", callback)
	run("config", "core.hooksPath", work)
	run("update-ref", "refs/heads/foreign", head)
	if err := os.MkdirAll(filepath.Join(workspace.Dir, "hooks"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Dir, "hooks", "post-checkout"), []byte("#!/bin/sh\necho escaped > "+marker+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	pack, err := ExportGitHead(context.Background(), workspace.Dir, base, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := ImportGitHead(context.Background(), work, base, head, pack); err != nil {
		t.Fatal(err)
	}
	if actual := isolationGit(t, work, nil, "rev-parse", "HEAD"); actual != head {
		t.Fatalf("repair not imported: %s", actual)
	}
	if actual := isolationGit(t, source, nil, "rev-parse", "refs/heads/foreign"); actual != base {
		t.Fatalf("foreign ref changed: %s", actual)
	}
	isolationGit(t, work, nil, "status", "--porcelain")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("sandbox callback ran on host: %v", err)
	}
	if diff := isolationGit(t, work, nil, "diff", "--cached", "--name-only"); diff != "" {
		t.Fatalf("host index not imported: %s", diff)
	}
	tree := run("rev-parse", head+"^{tree}")
	orphan := run("commit-tree", tree, "-m", "unrelated")
	pack, err = ExportGitHead(context.Background(), workspace.Dir, head, orphan)
	if err != nil {
		t.Fatal(err)
	}
	if err := ImportGitHead(context.Background(), work, head, orphan, pack); err == nil {
		t.Fatal("unrelated head accepted")
	}
	if actual, err := git.HeadSHA(context.Background(), work); err != nil || actual != head {
		t.Fatalf("refusal changed run head: %s %v", actual, err)
	}
	if err := ImportGitHead(context.Background(), work, head, strings.Repeat("z", 40), pack); err == nil {
		t.Fatal("non-object head accepted")
	}
	if err := os.WriteFile(filepath.Join(work, "staged-only"), []byte("custody bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("-c", "core.fsmonitor=false", "add", "staged-only")
	if err := os.Remove(filepath.Join(work, "staged-only")); err != nil {
		t.Fatal(err)
	}
	run("config", "core.fsmonitor", "false")
	index, err := ExportGitIndex(context.Background(), workspace.Dir, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := ImportGitIndex(context.Background(), work, index); err != nil {
		t.Fatal(err)
	}
	if got := isolationGit(t, work, nil, "show", ":staged-only"); got != "custody bytes" {
		t.Fatalf("staged-only repair lost: %q", got)
	}
	if _, err := os.Stat(filepath.Join(work, "staged-only")); !os.IsNotExist(err) {
		t.Fatalf("index import changed working files: %v", err)
	}
	if err := ImportGitIndex(context.Background(), work, &GitIndex{Pack: []byte("invalid")}); err == nil {
		t.Fatal("invalid index pack accepted")
	}
	if got := isolationGit(t, work, nil, "show", ":staged-only"); got != "custody bytes" {
		t.Fatal("failed import destroyed index")
	}
}

func TestNamespaceGitMetadataCannotMutateGateOrInstallHostCallbacks(t *testing.T) {
	worker := buildHelper(t)
	root, source, work, base := isolationGitFixture(t)
	workspace, err := PrepareGit(context.Background(), work, root)
	if err != nil {
		t.Fatal(err)
	}
	key, state := filepath.Join(root, "key"), filepath.Join(root, "state")
	if err := os.WriteFile(key, []byte("fixture-key"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, []byte("fixture-state"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	invocation, err := Start(ctx, Options{Worker: worker, Deck: worker, State: state, WorkDir: work, Git: workspace, ScratchParent: root, Model: ModelConfig{URL: "http://127.0.0.1:1/v1", KeyFile: key, Model: "fixture-model"}})
	if err != nil {
		t.Fatal(err)
	}
	defer invocation.Close()
	request, err := json.Marshal(map[string]string{"Mode": "git-metadata", "HiddenFile": filepath.Join(source, ".git"), "Canary": base})
	if err != nil {
		t.Fatal(err)
	}
	invocation.Command.Stdin = strings.NewReader(string(request))
	output, err := shellenv.CombinedOutputShellCommand(invocation.Command)
	if err != nil {
		t.Fatalf("Git boundary descendant: %v: %s", err, output)
	}
	var result struct {
		Head string
		Pack []byte
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode descendant: %v: %s", err, output)
	}
	if err := ImportGitHead(ctx, work, base, result.Head, result.Pack); err != nil {
		t.Fatal(err)
	}
	isolationGit(t, work, nil, "status", "--porcelain")
	if _, err := os.Stat(filepath.Join(work, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("host callback escaped: %v", err)
	}
	if got := isolationGit(t, source, nil, "rev-parse", "refs/heads/foreign"); got != base {
		t.Fatalf("shared ref mutated: %s", got)
	}
	if got := isolationGit(t, work, nil, "show", "HEAD:tracked"); got != "namespace repair" {
		t.Fatalf("repair lost: %s", got)
	}
}
