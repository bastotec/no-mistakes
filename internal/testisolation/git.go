package testisolation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

func runGitRaw(ctx context.Context, dir, input string, args ...string) ([]byte, error) {
	args = append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}, args...)
	return git.RunRawWithInput(ctx, dir, input, args...)
}

func runGit(ctx context.Context, dir, input string, args ...string) (string, error) {
	out, err := runGitRaw(ctx, dir, input, args...)
	return strings.TrimSpace(string(out)), err
}

type GitWorkspace struct {
	Dir      string
	Pointer  string
	Identity []string
}

func PrepareGit(ctx context.Context, workdir, root, fallbackBase string, integrationRefs ...string) (*GitWorkspace, error) {
	if err := CheckRepository(ctx, workdir, "HEAD"); err != nil {
		return nil, err
	}
	workspace := &GitWorkspace{Dir: filepath.Join(root, "git"), Pointer: filepath.Join(root, "git-pointer")}
	for _, role := range []string{"AUTHOR", "COMMITTER"} {
		ident, err := runGit(ctx, workdir, "", "var", "GIT_"+role+"_IDENT")
		if err != nil {
			return nil, fmt.Errorf("resolve isolated Git identity: %w", err)
		}
		start, end := strings.LastIndex(ident, " <"), strings.LastIndex(ident, ">")
		if start < 1 || end <= start+2 {
			return nil, fmt.Errorf("invalid resolved Git identity")
		}
		workspace.Identity = append(workspace.Identity, "GIT_"+role+"_NAME="+ident[:start], "GIT_"+role+"_EMAIL="+ident[start+2:end])
	}
	format, err := runGit(ctx, workdir, "", "rev-parse", "--show-object-format")
	if err != nil {
		return nil, err
	}
	if _, err := runGit(ctx, workdir, "", "init", "--bare", "--template=", "--object-format="+format, workspace.Dir); err != nil {
		return nil, err
	}
	config := "[core]\nrepositoryformatversion = 0\nbare = false\n"
	if format == "sha256" {
		config = "[core]\nrepositoryformatversion = 1\nbare = false\n[extensions]\nobjectformat = sha256\n"
	}
	if err := os.WriteFile(filepath.Join(workspace.Dir, "config"), []byte(config), 0600); err != nil {
		return nil, err
	}
	if _, err := runGit(ctx, workdir, "", "config", "--file", filepath.Join(workspace.Dir, "config"), "core.worktree", workdir); err != nil {
		return nil, err
	}
	checkoutConfig, err := runGitRaw(ctx, workdir, "", "config", "--null", "--get-regexp", `^core\.(autocrlf|eol|safecrlf|filemode|symlinks|ignorecase|precomposeunicode|checkroundtripencoding)$`)
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return nil, fmt.Errorf("resolve isolated Git checkout configuration: %w", err)
		}
	}
	for _, entry := range strings.Split(string(checkoutConfig), "\x00") {
		if entry == "" {
			continue
		}
		key, value, hasValue := strings.Cut(entry, "\n")
		if !hasValue {
			value = "true"
		}
		if _, err := runGit(ctx, workdir, "", "config", "--file", filepath.Join(workspace.Dir, "config"), "--replace-all", key, value); err != nil {
			return nil, err
		}
	}
	if err := prepareGitAttributes(ctx, workdir, workspace.Dir); err != nil {
		return nil, err
	}
	head, err := runGit(ctx, workdir, "", "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	revisions := []string{head}
	var integrationRef, integrationHead string
	for _, ref := range integrationRefs {
		if _, err := runGit(ctx, workdir, "", "merge-base", head, ref); err != nil {
			continue
		}
		integrationRef, err = runGit(ctx, workdir, "", "rev-parse", "--symbolic-full-name", "--verify", ref)
		if err != nil || !strings.HasPrefix(integrationRef, "refs/") {
			return nil, fmt.Errorf("resolve isolated Git integration ref: %s", ref)
		}
		integrationHead, err = runGit(ctx, workdir, "", "rev-parse", "--verify", integrationRef+"^{commit}")
		if err != nil {
			return nil, err
		}
		revisions = append(revisions, integrationHead)
		break
	}
	if integrationRef == "" && validHead(fallbackBase) && !git.IsZeroSHA(fallbackBase) {
		revisions = append(revisions, fallbackBase)
	}
	pack, err := runGitRaw(ctx, workdir, strings.Join(revisions, "\n")+"\n", "pack-objects", "--stdout", "--revs")
	if err != nil {
		return nil, err
	}
	if _, err := runGit(ctx, workdir, string(pack), "--git-dir="+workspace.Dir, "index-pack", "--stdin", "--strict"); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(workspace.Dir, "HEAD"), []byte(head+"\n"), 0600); err != nil {
		return nil, err
	}
	if integrationRef != "" {
		if _, err := runGit(ctx, workdir, "", "--git-dir="+workspace.Dir, "update-ref", integrationRef, integrationHead); err != nil {
			return nil, err
		}
	}
	index, err := ExportGitIndex(ctx, workdir, head)
	if err != nil {
		return nil, err
	}
	if err := importGitIndex(ctx, workspace.Dir, index); err != nil {
		return nil, err
	}
	if err := os.WriteFile(workspace.Pointer, []byte("gitdir: /nm/git\n"), 0600); err != nil {
		return nil, err
	}
	return workspace, nil
}

func prepareGitAttributes(ctx context.Context, workdir, privateDir string) error {
	external, err := runGit(ctx, workdir, "", "config", "--path", "--get", "core.attributesFile")
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return fmt.Errorf("resolve isolated Git attributes: %w", err)
		}
		configHome := os.Getenv("XDG_CONFIG_HOME")
		if configHome == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			configHome = filepath.Join(home, ".config")
		}
		external = filepath.Join(configHome, "git", "attributes")
	}
	info, err := runGit(ctx, workdir, "", "rev-parse", "--path-format=absolute", "--git-path", "info/attributes")
	if err != nil {
		return err
	}
	for _, file := range [][2]string{{external, filepath.Join(privateDir, "attributes")}, {info, filepath.Join(privateDir, "info", "attributes")}} {
		var data []byte
		if file[0] != "" {
			source := file[0]
			if !filepath.IsAbs(source) {
				source = filepath.Join(workdir, source)
			}
			data, err = os.ReadFile(source)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("read isolated Git attributes: %w", err)
			}
		}
		if err := os.MkdirAll(filepath.Dir(file[1]), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(file[1], data, 0600); err != nil {
			return err
		}
	}
	_, err = runGit(ctx, workdir, "", "config", "--file", filepath.Join(privateDir, "config"), "core.attributesFile", filepath.Join(privateDir, "attributes"))
	return err
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
	return runGitRaw(ctx, workdir, head+"\n^"+base+"\n", "pack-objects", "--stdout", "--revs")
}

func ImportGitHead(ctx context.Context, workdir, base, head string, pack []byte) error {
	if !validHead(base) || !validHead(head) {
		return fmt.Errorf("invalid isolated Git head")
	}
	if _, err := runGit(ctx, workdir, "", "symbolic-ref", "-q", "HEAD"); err == nil {
		return fmt.Errorf("isolated Test requires a detached run HEAD")
	}
	current, err := runGit(ctx, workdir, "", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if current != base {
		return fmt.Errorf("isolated Test run HEAD moved before import")
	}
	if _, err := runGit(ctx, workdir, string(pack), "index-pack", "--stdin", "--strict"); err != nil {
		return err
	}
	kind, err := runGit(ctx, workdir, "", "cat-file", "-t", head)
	if err != nil || kind != "commit" {
		return fmt.Errorf("isolated Test head is not a commit")
	}
	if _, err := runGit(ctx, workdir, "", "merge-base", "--is-ancestor", base, head); err != nil {
		return fmt.Errorf("isolated Test head rewrote run history: %w", err)
	}
	if err := CheckRepository(ctx, workdir, "HEAD", head); err != nil {
		return err
	}
	if _, err := runGit(ctx, workdir, "", "update-ref", "--no-deref", "HEAD", head, base); err != nil {
		return err
	}
	_, err = runGit(ctx, workdir, "", "read-tree", head)
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
	entries, err := runGitRaw(ctx, workdir, "", "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	objects, err := indexObjects(entries)
	if err != nil {
		return nil, err
	}
	revisions := strings.Join(append(objects, "^"+head), "\n") + "\n"
	pack, err := runGitRaw(ctx, workdir, revisions, "pack-objects", "--stdout", "--revs")
	if err != nil {
		return nil, err
	}
	return &GitIndex{Entries: entries, Pack: pack}, nil
}

func ImportGitIndex(ctx context.Context, workdir string, index *GitIndex) error {
	if err := CheckRepository(ctx, workdir, "HEAD"); err != nil {
		return err
	}
	return importGitIndex(ctx, workdir, index)
}

func importGitIndex(ctx context.Context, workdir string, index *GitIndex) error {
	if index == nil {
		return fmt.Errorf("isolated Test omitted its index handoff")
	}
	objects, err := indexObjects(index.Entries)
	if err != nil {
		return err
	}
	if _, err := runGit(ctx, workdir, string(index.Pack), "index-pack", "--stdin", "--strict"); err != nil {
		return err
	}
	if len(objects) != 0 {
		kinds, err := runGit(ctx, workdir, strings.Join(objects, "\n")+"\n", "cat-file", "--batch-check=%(objecttype)")
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
	head, err := runGit(ctx, workdir, "", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	paths, err := runGitRaw(ctx, workdir, "", "ls-files", "-z")
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
	_, err = runGit(ctx, workdir, entries.String(), "update-index", "-z", "--index-info")
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
			return nil, fmt.Errorf("test-isolation-repository: gitlinks/submodules are unsupported")
		default:
			return nil, fmt.Errorf("invalid isolated Git index mode")
		}
	}
	return objects, nil
}
