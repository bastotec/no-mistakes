package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type callbacksDisabledKey struct{}

func WithoutCallbacks(ctx context.Context) context.Context {
	return context.WithValue(ctx, callbacksDisabledKey{}, true)
}

func CallbacksDisabled(ctx context.Context) bool {
	return ctx.Value(callbacksDisabledKey{}) == true
}

func CallbackFreeArgs(ctx context.Context, dir string, args ...string) ([]string, error) {
	ctx = context.WithValue(ctx, callbacksDisabledKey{}, false)
	overrides := []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "submodule.recurse=false"}
	args = append([]string(nil), args...)
	for i, arg := range args {
		if strings.HasPrefix(arg, "--ignore-submodules=") {
			args[i] = "--ignore-submodules=all"
		}
	}
	keys, err := RunRaw(ctx, dir, append(overrides, "config", "--null", "--name-only", "--list")...)
	if err != nil {
		return nil, err
	}
	for _, key := range strings.Split(string(keys), "\x00") {
		field := key[strings.LastIndexByte(key, '.')+1:]
		if strings.HasPrefix(key, "filter.") && (field == "clean" || field == "smudge" || field == "process" || field == "required") ||
			strings.HasPrefix(key, "diff.") && field == "textconv" {
			if strings.ContainsAny(key, "=\n\r") {
				return nil, fmt.Errorf("cannot disable unsafe Git callback key")
			}
			value := ""
			if field == "required" {
				value = "false"
			}
			overrides = append(overrides, "-c", key+"="+value)
		}
	}
	return append(overrides, args...), nil
}

func WorktreeAddWithoutCallbacks(ctx context.Context, repoDir, wtPath, sha string) (err error) {
	if !filepath.IsAbs(wtPath) {
		wtPath, err = filepath.Abs(filepath.Join(repoDir, wtPath))
		if err != nil {
			return err
		}
	}
	args, err := CallbackFreeArgs(ctx, repoDir, "worktree", "add", "--no-checkout", "--detach", wtPath, sha)
	if err != nil {
		return err
	}
	if _, err = Run(ctx, repoDir, args...); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			cleanupCtx := context.WithoutCancel(ctx)
			cleanupArgs, cleanupErr := CallbackFreeArgs(cleanupCtx, repoDir, "worktree", "remove", "--force", wtPath)
			if cleanupErr == nil {
				_, cleanupErr = Run(cleanupCtx, repoDir, cleanupArgs...)
			}
			err = errors.Join(err, cleanupErr)
		}
	}()
	args, err = CallbackFreeArgs(ctx, wtPath, "reset", "--hard", "HEAD")
	if err != nil {
		return err
	}
	_, err = Run(ctx, wtPath, args...)
	return err
}

func FetchRemoteBranchToRefWithoutCallbacks(ctx context.Context, dir, remote, branch, localRef string) error {
	refspec := fmt.Sprintf("+refs/heads/%s:%s", branch, localRef)
	args, err := CallbackFreeArgs(ctx, dir, "fetch", "--no-tags", remote, refspec)
	if err != nil {
		return err
	}
	_, err = Run(ctx, dir, args...)
	return err
}

func DiffHeadWithoutCallbacks(ctx context.Context, dir string) (string, error) {
	args, err := CallbackFreeArgs(ctx, dir, "diff", "--no-ext-diff", "--no-textconv", "--ignore-submodules=all", "HEAD")
	if err != nil {
		return "", err
	}
	return Run(ctx, dir, args...)
}
