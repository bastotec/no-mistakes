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
	workspace, err := PrepareGit(context.Background(), work, root, base)
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
	for range 2 {
		next, err := PrepareGit(context.Background(), work, t.TempDir(), head)
		if err != nil {
			t.Fatalf("prepare subsequent invocation: %v", err)
		}
		if got := isolationGit(t, work, nil, "--git-dir="+next.Dir, "show", ":staged-only"); got != "custody bytes" {
			t.Fatalf("subsequent invocation lost staged-only blob: %q", got)
		}
		index, err := ExportGitIndex(context.Background(), next.Dir, head)
		if err != nil {
			t.Fatal(err)
		}
		if err := ImportGitIndex(context.Background(), work, index); err != nil {
			t.Fatal(err)
		}
	}
	if err := ImportGitIndex(context.Background(), work, &GitIndex{Pack: []byte("invalid")}); err == nil {
		t.Fatal("invalid index pack accepted")
	}
	if got := isolationGit(t, work, nil, "show", ":staged-only"); got != "custody bytes" {
		t.Fatal("failed import destroyed index")
	}
}

func TestIsolatedGitAttributesRejectEscapeAcrossHandoffs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		parent   bool
		absolute bool
		relative bool
		alias    bool
	}{
		{name: "relative-file"},
		{name: "absolute-file", absolute: true},
		{name: "relative-parent", parent: true},
		{name: "absolute-parent", parent: true, absolute: true},
		{name: "relative-symlink-target", relative: true},
		{name: "aliased-worktree", absolute: true, alias: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, source, work, base := isolationGitFixture(t)
			secretDir := t.TempDir()
			secret := filepath.Join(secretDir, "attrs")
			secretBytes := "host credential canary\n"
			if err := os.WriteFile(secret, []byte(secretBytes), 0600); err != nil {
				t.Fatal(err)
			}
			name := "attrs"
			if tc.parent {
				name = filepath.Join("rules", name)
			}
			attributePath := filepath.Join(work, name)
			if err := os.MkdirAll(filepath.Dir(attributePath), 0700); err != nil {
				t.Fatal(err)
			}
			initial := "tracked -text\n"
			if err := os.WriteFile(attributePath, []byte(initial), 0600); err != nil {
				t.Fatal(err)
			}
			configured := name
			if tc.absolute {
				configured = attributePath
			}
			isolationGit(t, source, nil, "config", "core.attributesFile", configured)
			if tc.alias {
				alias := filepath.Join(t.TempDir(), "work-alias")
				if err := os.Symlink(work, alias); err != nil {
					t.Fatal(err)
				}
				work = alias
			}
			workspace, err := PrepareGit(context.Background(), work, t.TempDir(), base)
			if err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(filepath.Join(workspace.Dir, "attributes")); err != nil || string(data) != initial {
				t.Fatalf("first handoff lost attributes: %q %v", data, err)
			}
			privateGit := func(args ...string) string {
				return isolationGit(t, work, append(Environment(work), workspace.Identity...), append([]string{"--git-dir=" + workspace.Dir}, args...)...)
			}
			privateGit("add", "--", name)
			privateGit("commit", "-m", "initial attributes")
			link, target := attributePath, secret
			if tc.parent {
				link, target = filepath.Dir(attributePath), secretDir
			}
			if tc.relative {
				target, err = filepath.Rel(filepath.Dir(link), target)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.RemoveAll(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			privateGit("add", "-A")
			privateGit("commit", "-m", "worker replaces attributes with symlink")
			head := privateGit("rev-parse", "HEAD")
			pack, err := ExportGitHead(context.Background(), workspace.Dir, base, head)
			if err != nil {
				t.Fatal(err)
			}
			if err := ImportGitHead(context.Background(), work, base, head, pack); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(attributePath); err != nil || string(data) != secretBytes {
				t.Fatalf("escape positive control: %q %v", data, err)
			}
			nextRoot := t.TempDir()
			if _, err := PrepareGit(context.Background(), work, nextRoot, head); err == nil || !strings.Contains(err.Error(), "read isolated Git attributes") {
				t.Fatalf("repeated handoff accepted escaped attributes: %v", err)
			}
			if data, err := os.ReadFile(filepath.Join(nextRoot, "git", "attributes")); err == nil && strings.Contains(string(data), secretBytes) {
				t.Fatal("host credential copied into next worker")
			}
		})
	}
}

func TestIsolatedGitAttributesPreserveAuthorizedSources(t *testing.T) {
	for _, mode := range []string{"relative-symlink", "external", "default"} {
		t.Run(mode, func(t *testing.T) {
			_, source, work, base := isolationGitFixture(t)
			attributes := "tracked text eol=lf\n"
			infoAttributes := "tracked -diff\n"
			path := filepath.Join(t.TempDir(), "attributes")
			switch mode {
			case "relative-symlink":
				path = filepath.Join(work, "attributes")
				if err := os.Symlink("attributes", filepath.Join(work, "attrs")); err != nil {
					t.Fatal(err)
				}
				isolationGit(t, source, nil, "config", "core.attributesFile", "attrs")
			case "external":
				isolationGit(t, source, nil, "config", "core.attributesFile", path)
			case "default":
				configHome := t.TempDir()
				t.Setenv("XDG_CONFIG_HOME", configHome)
				path = filepath.Join(configHome, "git", "attributes")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(attributes), 0600); err != nil {
				t.Fatal(err)
			}
			info := isolationGit(t, work, nil, "rev-parse", "--path-format=absolute", "--git-path", "info/attributes")
			if err := os.MkdirAll(filepath.Dir(info), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(info, []byte(infoAttributes), 0600); err != nil {
				t.Fatal(err)
			}
			workspace, err := PrepareGit(context.Background(), work, t.TempDir(), base)
			if err != nil {
				t.Fatal(err)
			}
			for name, expected := range map[string]string{"attributes": attributes, filepath.Join("info", "attributes"): infoAttributes} {
				if data, err := os.ReadFile(filepath.Join(workspace.Dir, name)); err != nil || string(data) != expected {
					t.Fatalf("authorized attributes %s: %q %v", name, data, err)
				}
			}
		})
	}
}

func TestIsolatedGitHostHandoffDisablesInheritedCallbacks(t *testing.T) {
	root, source, work, base := isolationGitFixture(t)
	hooks := filepath.Join(work, ".githooks")
	if err := os.Mkdir(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	markers := []string{"post-index-change", "reference-transaction", "fsmonitor"}
	for _, name := range markers {
		body := "#!/bin/sh\nprintf invoked > '" + filepath.Join(root, name) + "'\n"
		if err := os.WriteFile(filepath.Join(hooks, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	isolationGit(t, source, nil, "config", "core.hooksPath", ".githooks")
	isolationGit(t, source, nil, "config", "core.fsmonitor", filepath.Join(hooks, "fsmonitor"))
	isolationGit(t, work, nil, "update-index", "--force-write-index", "--refresh")
	isolationGit(t, work, nil, "update-ref", "refs/heads/callback-probe", base)
	for _, name := range markers {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("callback positive control %s: %v", name, err)
		}
		if err := os.Remove(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	workspace, err := PrepareGit(context.Background(), work, t.TempDir(), base)
	if err != nil {
		t.Fatal(err)
	}
	privateGit := func(args ...string) string {
		return isolationGit(t, work, append(Environment(work), workspace.Identity...), append([]string{"--git-dir=" + workspace.Dir}, args...)...)
	}
	if err := os.WriteFile(filepath.Join(work, "tracked"), []byte("private repair\n"), 0600); err != nil {
		t.Fatal(err)
	}
	privateGit("add", "tracked")
	privateGit("commit", "-m", "private repair")
	head := privateGit("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(work, "staged-only"), []byte("staged repair\n"), 0600); err != nil {
		t.Fatal(err)
	}
	privateGit("add", "staged-only")
	pack, err := ExportGitHead(context.Background(), workspace.Dir, base, head)
	if err != nil {
		t.Fatal(err)
	}
	index, err := ExportGitIndex(context.Background(), workspace.Dir, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := ImportGitHead(context.Background(), work, base, head, pack); err != nil {
		t.Fatal(err)
	}
	if err := ImportGitIndex(context.Background(), work, index); err != nil {
		t.Fatal(err)
	}
	for _, name := range markers {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("host callback %s ran across isolation boundary: %v", name, err)
		}
	}
	if got := isolationGit(t, work, nil, "rev-parse", "HEAD"); got != head {
		t.Fatalf("head handoff lost: %s", got)
	}
	if got := isolationGit(t, work, nil, "-c", "core.fsmonitor=false", "show", ":staged-only"); got != "staged repair" {
		t.Fatalf("index handoff lost: %q", got)
	}
}

func TestIsolatedGitTransfersOnlyRequiredHistory(t *testing.T) {
	_, source, work, base := isolationGitFixture(t)
	if err := os.WriteFile(filepath.Join(source, "integration"), []byte("integration bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	isolationGit(t, source, nil, "add", "integration")
	isolationGit(t, source, nil, "commit", "-m", "integration advance")
	tip := isolationGit(t, source, nil, "rev-parse", "HEAD")
	isolationGit(t, source, nil, "update-ref", "refs/remotes/origin/main", tip)
	isolationGit(t, source, nil, "reset", "--hard", base)
	if err := os.WriteFile(filepath.Join(source, "archive"), []byte("unrelated recovery bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	isolationGit(t, source, nil, "add", "archive")
	blob := isolationGit(t, source, nil, "rev-parse", ":archive")
	tree := isolationGit(t, source, nil, "write-tree")
	archive := isolationGit(t, source, nil, "commit-tree", tree, "-m", "unrelated recovery")
	isolationGit(t, source, nil, "update-ref", "refs/no-mistakes/recovery/unrelated", archive)
	isolationGit(t, source, nil, "reset", "--hard", base)

	for _, test := range []struct {
		name string
		refs []string
		want string
	}{
		{"integration", []string{"origin/main", "foreign"}, "refs/remotes/origin/main"},
		{"fallback", []string{"missing"}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace, err := PrepareGit(context.Background(), work, t.TempDir(), tip, test.refs...)
			if err != nil {
				t.Fatal(err)
			}
			run := func(args ...string) string {
				return isolationGit(t, work, nil, append([]string{"--git-dir=" + workspace.Dir}, args...)...)
			}
			if got := run("rev-parse", "HEAD"); got != base {
				t.Fatalf("run head changed: %s", got)
			}
			if got := run("for-each-ref", "--format=%(refname)"); got != test.want {
				t.Fatalf("unexpected private refs: %q", got)
			}
			if got := run("show", tip+":integration"); got != "integration bytes" {
				t.Fatalf("required integration history lost: %q", got)
			}
			for _, object := range []string{blob, archive} {
				got, err := git.RunWithInput(context.Background(), workspace.Dir, object+"\n", "cat-file", "--batch-check")
				if err != nil || got != object+" missing" {
					t.Fatalf("unrelated recovery object transferred: %q %v", got, err)
				}
			}
			if _, err := os.Stat(filepath.Join(workspace.Dir, "objects", "info", "alternates")); !os.IsNotExist(err) {
				t.Fatalf("private store references shared objects: %v", err)
			}
		})
	}
}

func TestNamespaceGitMetadataCannotMutateGateOrInstallHostCallbacks(t *testing.T) {
	worker := buildHelper(t)
	root, source, work, base := isolationGitFixture(t)
	workspace, err := PrepareGit(context.Background(), work, root, base)
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
