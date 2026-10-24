package git

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

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
