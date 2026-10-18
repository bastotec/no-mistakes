package testisolation

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// CheckRepository admits only a single working tree. It never runs Git inside
// a nested repository, follows symlinks, or relies on recursive status/config
// inspection. Call only after the isolated descendants have been reaped.
func CheckRepository(ctx context.Context, dir string, refs ...string) error {
	entries, err := runGitRaw(ctx, dir, "", "ls-files", "--stage", "-z")
	if err != nil {
		return err
	}
	if _, err := indexObjects(entries); err != nil {
		return err
	}
	for _, ref := range refs {
		if err := CheckRepositoryTree(ctx, dir, ref); err != nil {
			return err
		}
	}
	root, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	left := 100000
	return scanRepository(ctx, root, 0, &left)
}

// CheckRepositoryTree reads object metadata only, before a candidate tree can
// replace the run's HEAD/index or a captured layer can be restored.
func CheckRepositoryTree(ctx context.Context, dir, ref string) error {
	tree, err := runGitRaw(ctx, dir, "", "ls-tree", "-r", "-t", "-z", ref)
	if err != nil {
		return err
	}
	heads, objects := map[string]bool{}, map[string]bool{}
	for _, entry := range strings.Split(string(tree), "\x00") {
		if strings.HasPrefix(entry, "160000 ") {
			return fmt.Errorf("test-isolation-repository: gitlinks/submodules are unsupported")
		}
		_, name, ok := strings.Cut(entry, "\t")
		if !ok {
			continue
		}
		for _, part := range strings.Split(name, "/") {
			if part == ".git" {
				return fmt.Errorf("test-isolation-repository: nested Git metadata is unsupported")
			}
		}
		if path.Base(name) == "HEAD" {
			heads[path.Dir(name)] = true
		}
		if path.Base(name) == "objects" {
			objects[path.Dir(name)] = true
		}
	}
	for dir := range heads {
		if objects[dir] {
			return fmt.Errorf("test-isolation-repository: nested bare repository is unsupported")
		}
	}
	return nil
}

func scanRepository(ctx context.Context, f *os.File, depth int, left *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 128 {
		return fmt.Errorf("test-isolation-repository: inspection depth exceeded")
	}
	hasHead, hasObjects := false, false
	for {
		entries, err := f.ReadDir(128)
		if err != nil && err != io.EOF {
			return err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			*left--
			if *left < 0 {
				return fmt.Errorf("test-isolation-repository: inspection entry limit exceeded")
			}
			hasHead = hasHead || entry.Name() == "HEAD"
			hasObjects = hasObjects || entry.Name() == "objects"
			if depth > 0 && hasHead && hasObjects {
				return fmt.Errorf("test-isolation-repository: nested bare repository is unsupported")
			}
			if entry.Name() == ".git" {
				if depth == 0 {
					continue
				}
				return fmt.Errorf("test-isolation-repository: nested Git metadata is unsupported")
			}
			if entry.IsDir() {
				// Pin the directory by descriptor without following links. A
				// concurrent rename/replacement cannot redirect inspection into
				// a host directory, including during a live head acknowledgement.
				child, err := openRepositoryDir(f, entry.Name())
				if err != nil {
					return err
				}
				err = scanRepository(ctx, child, depth+1, left)
				child.Close()
				if err != nil {
					return err
				}
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}
