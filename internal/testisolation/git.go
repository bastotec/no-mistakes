package testisolation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

type GitWorkspace struct {
	Dir      string
	Pointer  string
	Identity []string
}

func PrepareGit(ctx context.Context, workdir, root string) (*GitWorkspace, error) {
	workspace := &GitWorkspace{Dir: filepath.Join(root, "git"), Pointer: filepath.Join(root, "git-pointer")}
	for _, role := range []string{"AUTHOR", "COMMITTER"} {
		ident, err := git.Run(ctx, workdir, "var", "GIT_"+role+"_IDENT")
		if err != nil {
			return nil, fmt.Errorf("resolve isolated Git identity: %w", err)
		}
		start, end := strings.LastIndex(ident, " <"), strings.LastIndex(ident, ">")
		if start < 1 || end <= start+2 {
			return nil, fmt.Errorf("invalid resolved Git identity")
		}
		workspace.Identity = append(workspace.Identity, "GIT_"+role+"_NAME="+ident[:start], "GIT_"+role+"_EMAIL="+ident[start+2:end])
	}
	if _, err := git.Run(ctx, workdir, "clone", "--mirror", "--no-local", "--template=", "--", workdir, workspace.Dir); err != nil {
		return nil, err
	}
	format, err := git.Run(ctx, workdir, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, err
	}
	config := "[core]\nrepositoryformatversion = 0\nbare = false\n"
	if format == "sha256" {
		config = "[core]\nrepositoryformatversion = 1\nbare = false\n[extensions]\nobjectformat = sha256\n"
	}
	if err := os.WriteFile(filepath.Join(workspace.Dir, "config"), []byte(config), 0600); err != nil {
		return nil, err
	}
	if _, err := git.Run(ctx, workdir, "config", "--file", filepath.Join(workspace.Dir, "config"), "core.worktree", workdir); err != nil {
		return nil, err
	}
	head, err := git.HeadSHA(ctx, workdir)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(workspace.Dir, "HEAD"), []byte(head+"\n"), 0600); err != nil {
		return nil, err
	}
	index, err := git.RunRaw(ctx, workdir, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	if _, err := git.RunWithInput(ctx, workdir, string(index), "--git-dir="+workspace.Dir, "update-index", "-z", "--index-info"); err != nil {
		return nil, err
	}
	if err := os.WriteFile(workspace.Pointer, []byte("gitdir: /nm/git\n"), 0600); err != nil {
		return nil, err
	}
	return workspace, nil
}

func validHead(head string) bool {
	if len(head) != 40 && len(head) != 64 {
		return false
	}
	for _, r := range head {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func ExportGitHead(ctx context.Context, workdir, base, head string) ([]byte, error) {
	if !validHead(base) || !validHead(head) {
		return nil, fmt.Errorf("invalid isolated Git head")
	}
	return git.RunRawWithInput(ctx, workdir, head+"\n^"+base+"\n", "pack-objects", "--stdout", "--revs")
}

func ImportGitHead(ctx context.Context, workdir, base, head string, pack []byte) error {
	if !validHead(base) || !validHead(head) {
		return fmt.Errorf("invalid isolated Git head")
	}
	if _, err := git.Run(ctx, workdir, "symbolic-ref", "-q", "HEAD"); err == nil {
		return fmt.Errorf("isolated Test requires a detached run HEAD")
	}
	current, err := git.HeadSHA(ctx, workdir)
	if err != nil {
		return err
	}
	if current != base {
		return fmt.Errorf("isolated Test run HEAD moved before import")
	}
	if _, err := git.RunWithInput(ctx, workdir, string(pack), "index-pack", "--stdin", "--strict"); err != nil {
		return err
	}
	kind, err := git.Run(ctx, workdir, "cat-file", "-t", head)
	if err != nil || kind != "commit" {
		return fmt.Errorf("isolated Test head is not a commit")
	}
	if _, err := git.Run(ctx, workdir, "merge-base", "--is-ancestor", base, head); err != nil {
		return fmt.Errorf("isolated Test head rewrote run history: %w", err)
	}
	if _, err := git.Run(ctx, workdir, "update-ref", "--no-deref", "HEAD", head, base); err != nil {
		return err
	}
	_, err = git.Run(ctx, workdir, "read-tree", head)
	return err
}

type GitIndex struct {
	Entries []byte
	Pack    []byte
}

func ExportGitIndex(ctx context.Context, workdir, head string) (*GitIndex, error) {
	if !validHead(head) {
		return nil, fmt.Errorf("invalid isolated Git head")
	}
	entries, err := git.RunRaw(ctx, workdir, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	objects, err := indexObjects(entries)
	if err != nil {
		return nil, err
	}
	revisions := strings.Join(append(objects, "^"+head), "\n") + "\n"
	pack, err := git.RunRawWithInput(ctx, workdir, revisions, "pack-objects", "--stdout", "--revs")
	if err != nil {
		return nil, err
	}
	return &GitIndex{Entries: entries, Pack: pack}, nil
}

func ImportGitIndex(ctx context.Context, workdir string, index *GitIndex) error {
	if index == nil {
		return fmt.Errorf("isolated Test omitted its index handoff")
	}
	objects, err := indexObjects(index.Entries)
	if err != nil {
		return err
	}
	if _, err := git.RunWithInput(ctx, workdir, string(index.Pack), "index-pack", "--stdin", "--strict"); err != nil {
		return err
	}
	if len(objects) != 0 {
		kinds, err := git.RunWithInput(ctx, workdir, strings.Join(objects, "\n")+"\n", "cat-file", "--batch-check=%(objecttype)")
		if err != nil {
			return err
		}
		lines := strings.Split(kinds, "\n")
		if len(lines) != len(objects) {
			return fmt.Errorf("invalid isolated Git index objects")
		}
		for _, kind := range lines {
			if kind != "blob" {
				return fmt.Errorf("isolated Git index references a non-blob")
			}
		}
	}
	head, err := git.HeadSHA(ctx, workdir)
	if err != nil {
		return err
	}
	paths, err := git.RunRaw(ctx, workdir, "ls-files", "-z")
	if err != nil {
		return err
	}
	var entries strings.Builder
	for _, path := range strings.Split(string(paths), "\x00") {
		if path != "" {
			entries.WriteString("0 " + strings.Repeat("0", len(head)) + "\t" + path + "\x00")
		}
	}
	entries.Write(index.Entries)
	_, err = git.RunWithInput(ctx, workdir, entries.String(), "update-index", "-z", "--index-info")
	return err
}

func indexObjects(entries []byte) ([]string, error) {
	var objects []string
	for _, entry := range strings.Split(string(entries), "\x00") {
		if entry == "" {
			continue
		}
		header, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(header)
		if !ok || path == "" || len(fields) != 3 || !validHead(fields[1]) || (fields[2] != "0" && fields[2] != "1" && fields[2] != "2" && fields[2] != "3") {
			return nil, fmt.Errorf("invalid isolated Git index entry")
		}
		switch fields[0] {
		case "100644", "100755", "120000":
			objects = append(objects, fields[1])
		case "160000":
		default:
			return nil, fmt.Errorf("invalid isolated Git index mode")
		}
	}
	return objects, nil
}
