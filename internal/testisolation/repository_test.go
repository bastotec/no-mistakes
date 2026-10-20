package testisolation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryAdmissionRejectsGitlinkHandoffsWithoutMutation(t *testing.T) {
	root, _, work, base := isolationGitFixture(t)
	ctx := context.Background()
	before := isolationGit(t, work, nil, "ls-files", "--stage")
	index := &GitIndex{Entries: []byte("160000 " + base + " 0\tsubmodule\x00")}
	if err := ImportGitIndex(ctx, work, index); err == nil {
		t.Fatal("gitlink index accepted")
	}
	if got := isolationGit(t, work, nil, "ls-files", "--stage"); got != before {
		t.Fatal("refusal changed index")
	}
	workspace, err := PrepareGit(ctx, work, root, base)
	if err != nil {
		t.Fatal(err)
	}
	env := append(Environment(work), workspace.Identity...)
	run := func(args ...string) string {
		return isolationGit(t, work, env, append([]string{"--git-dir=" + workspace.Dir}, args...)...)
	}
	run("update-index", "--add", "--cacheinfo", "160000,"+base+",submodule")
	run("commit", "-m", "unsupported gitlink")
	head := run("rev-parse", "HEAD")
	pack, err := ExportGitHead(ctx, workspace.Dir, base, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := ImportGitHead(ctx, work, base, head, pack); err == nil {
		t.Fatal("gitlink head accepted")
	}
	if got := isolationGit(t, work, nil, "rev-parse", "HEAD"); got != base {
		t.Fatal("refusal changed HEAD")
	}
	if got := isolationGit(t, work, nil, "ls-files", "--stage"); got != before {
		t.Fatal("head refusal changed index")
	}
}

func TestRepositoryAdmissionRefusesNestedAndHonorsBounds(t *testing.T) {
	for _, kind := range []string{"directory", "git-file", "bare", "symlink", "cancellation", "depth", "entries"} {
		t.Run(kind, func(t *testing.T) {
			_, _, work, _ := isolationGitFixture(t)
			ctx := context.Background()
			nested := filepath.Join(work, "nested")
			if err := os.Mkdir(nested, 0700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "directory":
				if err := os.Mkdir(filepath.Join(nested, ".git"), 0700); err != nil {
					t.Fatal(err)
				}
			case "git-file":
				if err := os.WriteFile(filepath.Join(nested, ".git"), []byte("gitdir: /unavailable\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "bare":
				isolationGit(t, work, nil, "init", "--bare", nested)
			case "symlink":
				// A symlink into unrelated storage must not be followed by admission.
				target := t.TempDir()
				if err := os.Mkdir(filepath.Join(target, ".git"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(nested, "link")); err != nil {
					t.Fatal(err)
				}
			case "cancellation":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			case "depth":
				if err := os.MkdirAll(filepath.Join(nested, strings.Repeat("d/", 130)), 0700); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			if kind == "entries" {
				f, e := os.Open(work)
				if e != nil {
					t.Fatal(e)
				}
				defer f.Close()
				left := 0
				err = scanRepository(ctx, f, 0, &left)
			} else {
				err = CheckRepository(ctx, work, "HEAD")
			}
			if kind == "symlink" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unsupported admission accepted")
			}
			if _, err := os.Stat(nested); err != nil {
				t.Fatal("inspection mutated work:", err)
			}
		})
	}
}
