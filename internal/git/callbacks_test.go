package git

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestWorktreeAddWithoutCallbacksConditionalConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX callback fixture")
	}
	for _, confined := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "confined"}[confined], func(t *testing.T) {
			source := initTestRepo(t)
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "callback")
			if err := os.Mkdir(filepath.Join(source, "tools"), 0755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(source, "tools", "filter.sh"), "#!/bin/sh\nprintf called >> '"+marker+"'\ncat\n")
			writeFile(t, filepath.Join(source, ".gitattributes"), "z-filtered.txt filter=conditional\n")
			writeFile(t, filepath.Join(source, "z-filtered.txt"), "payload\n")
			run(t, source, "git", "add", ".")
			run(t, source, "git", "commit", "-m", "submitted callback")
			gate := filepath.Join(root, "gate.git")
			run(t, source, "git", "clone", "--bare", source, gate)
			wt := filepath.Join(root, "worker")
			callback := "sh '" + filepath.ToSlash(filepath.Join(wt, "tools", "filter.sh")) + "'"
			include := filepath.Join(root, "conditional.gitconfig")
			writeFile(t, include, "[filter \"conditional\"]\nsmudge = "+strconv.Quote(callback)+"\nrequired = true\n")
			global := filepath.Join(root, "global.gitconfig")
			pattern := "gitdir:" + filepath.ToSlash(gate) + "/worktrees/**"
			writeFile(t, global, "[includeIf "+strconv.Quote(pattern)+"]\npath = "+strconv.Quote(filepath.ToSlash(include))+"\n")
			t.Setenv("GIT_CONFIG_GLOBAL", global)
			if _, err := Run(context.Background(), gate, "config", "--get", "filter.conditional.smudge"); err == nil {
				t.Fatal("conditional callback unexpectedly active in bare gate")
			}
			add := WorktreeAdd
			if confined {
				add = WorktreeAddWithoutCallbacks
			}
			if err := add(context.Background(), gate, wt, "HEAD"); err != nil {
				t.Fatal(err)
			}
			if got := run(t, wt, "git", "config", "--get", "filter.conditional.smudge"); got != callback {
				t.Fatalf("worktree did not activate conditional callback: %q", got)
			}
			_, err = os.Stat(marker)
			if confined && !os.IsNotExist(err) || !confined && err != nil {
				t.Fatalf("conditional callback confined=%v: %v", confined, err)
			}
			if data, err := os.ReadFile(filepath.Join(wt, "z-filtered.txt")); err != nil || string(data) != "payload\n" {
				t.Fatalf("checkout failed: %q %v", data, err)
			}
			if diff, err := DiffHeadWithoutCallbacks(context.Background(), wt); err != nil || strings.TrimSpace(diff) != "" {
				t.Fatalf("checkout differs from submitted tree: %q %v", diff, err)
			}
		})
	}
}

func TestWorktreeAddWithoutCallbacks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX callback fixture")
	}
	for _, confined := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "confined"}[confined], func(t *testing.T) {
			dir := initTestRepo(t)
			writeFile(t, filepath.Join(dir, ".gitattributes"), "filtered.txt filter=evil\n")
			writeFile(t, filepath.Join(dir, "filtered.txt"), "payload\n")
			run(t, dir, "git", "add", ".")
			run(t, dir, "git", "commit", "-m", "filtered payload")
			markers := t.TempDir()
			smudge := filepath.Join(markers, "smudge")
			hook := filepath.Join(markers, "hook")
			run(t, dir, "git", "config", "filter.evil.smudge", "printf called >> '"+smudge+"'; cat")
			run(t, dir, "git", "config", "filter.evil.required", "true")
			hooks := t.TempDir()
			if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\nprintf called >> '"+hook+"'\n"), 0755); err != nil {
				t.Fatal(err)
			}
			run(t, dir, "git", "config", "core.hooksPath", hooks)
			wt := filepath.Join(t.TempDir(), "worker")
			add := WorktreeAdd
			if confined {
				add = WorktreeAddWithoutCallbacks
			}
			if err := add(context.Background(), dir, wt, "HEAD"); err != nil {
				t.Fatal(err)
			}
			for _, marker := range []string{smudge, hook} {
				_, err := os.Stat(marker)
				if confined && !os.IsNotExist(err) || !confined && err != nil {
					t.Fatalf("callback %s confined=%v: %v", marker, confined, err)
				}
			}
			if data, err := os.ReadFile(filepath.Join(wt, "filtered.txt")); err != nil || string(data) != "payload\n" {
				t.Fatalf("checkout failed: %q %v", data, err)
			}
		})
	}
}
