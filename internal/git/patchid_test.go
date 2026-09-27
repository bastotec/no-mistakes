package git

import (
	"context"
	"path/filepath"
	"testing"
)

// patchRepo builds a linear history on an initial base commit and hands back
// small commit helpers. File content is written and committed in one step so a
// fixture can replay the same change under different SHAs by repeating it.
type patchRepo struct {
	t   *testing.T
	dir string
}

func newPatchRepo(t *testing.T) *patchRepo {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "init")
	run(t, dir, "git", "config", "user.email", "test@test.com")
	run(t, dir, "git", "config", "user.name", "Test")
	run(t, dir, "git", "config", "core.autocrlf", "false")
	run(t, dir, "git", "checkout", "-b", "main")
	writeFile(t, filepath.Join(dir, "feature.txt"), "one\ntwo\nthree\nfour\nfive\nsix\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "base")
	return &patchRepo{t: t, dir: dir}
}

func (p *patchRepo) commit(file, content, message string) string {
	p.t.Helper()
	writeFile(p.t, filepath.Join(p.dir, file), content)
	run(p.t, p.dir, "git", "add", ".")
	run(p.t, p.dir, "git", "commit", "-m", message)
	return run(p.t, p.dir, "git", "rev-parse", "HEAD")
}

func (p *patchRepo) checkoutNew(branch, start string) {
	p.t.Helper()
	run(p.t, p.dir, "git", "checkout", "-b", branch, start)
}

func (p *patchRepo) checkout(branch string) {
	p.t.Helper()
	run(p.t, p.dir, "git", "checkout", branch)
}

// TestComparePatchRetention_RebasedSeriesWithEquivalentPatches is the core
// reconciliation the content-identity comparison exists for: a supported
// rebase rewrites every SHA while replaying the same patches, and the private
// side's changes must read as retained even though no commit object is
// contained.
func TestComparePatchRetention_RebasedSeriesWithEquivalentPatches(t *testing.T) {
	p := newPatchRepo(t)
	base := run(t, p.dir, "git", "rev-parse", "HEAD")

	p.checkoutNew("private", base)
	first := p.commit("feature.txt", "one\ntwo\nthree\nfour-priv\nfive\nsix\n", "first private change")
	second := p.commit("extra.txt", "extra\n", "second private change")
	privateHead := second

	p.checkoutNew("live", base)
	p.commit("upstream.txt", "upstream advance\n", "upstream advance")
	p.commit("feature.txt", "one\ntwo\nthree\nfour-priv\nfive\nsix\n", "first private change replayed")
	p.commit("extra.txt", "extra\n", "second private change replayed")
	liveHead := run(t, p.dir, "git", "rev-parse", "HEAD")

	if first == "" || second == "" {
		t.Fatal("fixture produced no private commits")
	}
	retention, err := ComparePatchRetention(context.Background(), p.dir, liveHead, privateHead)
	if err != nil {
		t.Fatal(err)
	}
	if !retention.RetainsAll() {
		t.Fatalf("rebased equivalent series not retained: %+v", retention)
	}
	if len(retention.Retained) != 2 {
		t.Fatalf("retained = %v, want both private commits", retention.Retained)
	}
}

// TestComparePatchRetention_ReplayedThenSupersededPatchIsRetained pins the
// boundary whole-tree survival cannot prove: the live side replays the private
// patch and then a later live commit supersedes the same lines (a pipeline
// fix). The 3-way merge conflicts by construction, but the private change was
// carried, so patch identity must reconcile it.
func TestComparePatchRetention_ReplayedThenSupersededPatchIsRetained(t *testing.T) {
	p := newPatchRepo(t)
	base := run(t, p.dir, "git", "rev-parse", "HEAD")

	p.checkoutNew("private", base)
	privateHead := p.commit("feature.txt", "one\ntwo\nthree\nfour-priv\nfive\nsix\n", "private change")

	p.checkoutNew("live", base)
	p.commit("upstream.txt", "upstream advance\n", "upstream advance")
	p.commit("feature.txt", "one\ntwo\nthree\nfour-priv\nfive\nsix\n", "private change replayed")
	p.commit("feature.txt", "one\ntwo\nthree\nfour-fixed\nfive\nsix\n", "pipeline fix supersedes the line")
	liveHead := run(t, p.dir, "git", "rev-parse", "HEAD")

	retention, err := ComparePatchRetention(context.Background(), p.dir, liveHead, privateHead)
	if err != nil {
		t.Fatal(err)
	}
	if !retention.RetainsAll() {
		t.Fatalf("replayed-then-superseded patch not retained: %+v", retention)
	}
}

// TestComparePatchRetention_TrueContentLossIsUnretained: one private change is
// replayed, another is dropped entirely. Only the dropped one is unretained,
// and the comparison still decides (no structural decline).
func TestComparePatchRetention_TrueContentLossIsUnretained(t *testing.T) {
	p := newPatchRepo(t)
	base := run(t, p.dir, "git", "rev-parse", "HEAD")

	p.checkoutNew("private", base)
	p.commit("feature.txt", "one\ntwo\nthree\nfour-priv\nfive\nsix\n", "replayed change")
	dropped := p.commit("dropped.txt", "dropped work\n", "dropped change")
	privateHead := dropped

	p.checkoutNew("live", base)
	p.commit("upstream.txt", "upstream advance\n", "upstream advance")
	p.commit("feature.txt", "one\ntwo\nthree\nfour-priv\nfive\nsix\n", "replayed change replayed")
	liveHead := run(t, p.dir, "git", "rev-parse", "HEAD")

	retention, err := ComparePatchRetention(context.Background(), p.dir, liveHead, privateHead)
	if err != nil {
		t.Fatal(err)
	}
	if !retention.Comparable {
		t.Fatalf("comparison declined on an ordinary range: %+v", retention)
	}
	if retention.RetainsAll() {
		t.Fatalf("dropped change read as retained: %+v", retention)
	}
	if len(retention.Unretained) != 1 || retention.Unretained[0] != dropped {
		t.Fatalf("unretained = %v, want only the dropped commit %s", retention.Unretained, dropped)
	}
}

func TestComparePatchRetention_PrivateRangeRemainsStrict(t *testing.T) {
	for _, shape := range []string{"merge", "empty", "duplicate"} {
		t.Run(shape, func(t *testing.T) {
			p := newPatchRepo(t)
			base := run(t, p.dir, "git", "rev-parse", "HEAD")
			p.checkoutNew("private", base)
			p.commit("feature.txt", "one\ntwo\nthree\nfour-priv\nfive\nsix\n", "private change")
			switch shape {
			case "merge":
				privateTip := run(t, p.dir, "git", "rev-parse", "HEAD")
				p.checkoutNew("private-side", base)
				p.commit("side.txt", "side\n", "private side work")
				p.checkout("private")
				p.commit("main.txt", "main\n", "private main work")
				run(t, p.dir, "git", "merge", "--no-ff", "-m", "private merge", "private-side")
				if privateTip == "" {
					t.Fatal("fixture produced no private tip")
				}
			case "empty":
				run(t, p.dir, "git", "commit", "--allow-empty", "-m", "empty private commit")
			case "duplicate":
				p.commit("dup.txt", "dup\n", "duplicate one")
				run(t, p.dir, "git", "rm", "dup.txt")
				run(t, p.dir, "git", "commit", "-m", "drop duplicate")
				p.commit("dup.txt", "dup\n", "duplicate two")
			}
			privateHead := run(t, p.dir, "git", "rev-parse", "HEAD")

			p.checkoutNew("live", base)
			p.commit("feature.txt", "one\ntwo\nthree\nfour-priv\nfive\nsix\n", "private change replayed")
			liveHead := run(t, p.dir, "git", "rev-parse", "HEAD")

			retention, err := ComparePatchRetention(context.Background(), p.dir, liveHead, privateHead)
			if err != nil {
				t.Fatal(err)
			}
			if retention.Comparable || retention.RetainsAll() || len(retention.Retained) != 0 {
				t.Fatalf("strict private range read as comparable: %+v", retention)
			}
		})
	}
}

func TestComparePatchRetention_UnrelatedLiveStructureAndExtrasDoNotPoisonReplay(t *testing.T) {
	p := newPatchRepo(t)
	base := run(t, p.dir, "git", "rev-parse", "HEAD")
	p.checkoutNew("private", base)
	privateHead := p.commit("feature.txt", "one\ntwo\nthree\nfour-priv\nfive\nsix\n", "private change")

	p.checkoutNew("live", base)
	p.commit("upstream.txt", "upstream\n", "upstream main")
	liveBeforeMerge := run(t, p.dir, "git", "rev-parse", "HEAD")
	p.checkoutNew("upstream-side", base)
	p.commit("side.txt", "side\n", "upstream side")
	p.checkout("live")
	run(t, p.dir, "git", "merge", "--no-ff", "-m", "unrelated upstream merge", "upstream-side")
	run(t, p.dir, "git", "commit", "--allow-empty", "-m", "live empty metadata")
	p.commit("extra.txt", "duplicate extra\n", "extra patch one")
	run(t, p.dir, "git", "rm", "extra.txt")
	run(t, p.dir, "git", "commit", "-m", "remove extra patch")
	p.commit("extra.txt", "duplicate extra\n", "extra patch two")
	p.commit("feature.txt", "one\ntwo\nthree\nfour-priv\nfive\nsix\n", "private change replayed")
	p.commit("feature.txt", "one\ntwo\nthree\nfour-fixed\nfive\nsix\n", "later same-hunk fix")
	liveHead := run(t, p.dir, "git", "rev-parse", "HEAD")
	if liveBeforeMerge == "" {
		t.Fatal("fixture produced no pre-merge head")
	}

	retention, err := ComparePatchRetention(context.Background(), p.dir, liveHead, privateHead)
	if err != nil {
		t.Fatal(err)
	}
	if !retention.RetainsAll() {
		t.Fatalf("unrelated live history poisoned equivalent replay: %+v", retention)
	}
}

func TestComparePatchRetention_LiveStructuralOverlapFailsClosed(t *testing.T) {
	p := newPatchRepo(t)
	base := run(t, p.dir, "git", "rev-parse", "HEAD")
	p.checkoutNew("private", base)
	privateHead := p.commit("feature.txt", "one\ntwo\nthree\nfour-priv\nfive\nsix\n", "private change")

	p.checkoutNew("live", base)
	p.commit("feature.txt", "one\ntwo\nthree\nfour-priv\nfive\nsix\n", "private change replayed")
	mergeBase := run(t, p.dir, "git", "rev-parse", "HEAD")
	p.checkoutNew("live-side", mergeBase)
	p.commit("feature.txt", "one\ntwo\nthree\nfour-side\nfive\nsix\n", "side changes required path")
	p.checkout("live")
	p.commit("main.txt", "main\n", "force merge")
	run(t, p.dir, "git", "merge", "--no-ff", "-m", "overlapping merge", "live-side")
	liveHead := run(t, p.dir, "git", "rev-parse", "HEAD")

	retention, err := ComparePatchRetention(context.Background(), p.dir, liveHead, privateHead)
	if err != nil {
		t.Fatal(err)
	}
	if retention.Comparable || retention.RetainsAll() {
		t.Fatalf("overlapping structural commit read as comparable: %+v", retention)
	}
}

func TestComparePatchRetention_OrderedDuplicateExtraIsUnambiguous(t *testing.T) {
	p := newPatchRepo(t)
	base := run(t, p.dir, "git", "rev-parse", "HEAD")
	p.checkoutNew("private", base)
	p.commit("feature.txt", "one\ntwo\nthree\nfour-priv\nfive\nsix\n", "private P")
	privateHead := p.commit("required-q.txt", "q\n", "private Q")

	p.checkoutNew("live", base)
	firstP := p.commit("feature.txt", "one\ntwo\nthree\nfour-priv\nfive\nsix\n", "replay P")
	p.commit("required-q.txt", "q\n", "replay Q")
	run(t, p.dir, "git", "revert", "--no-edit", firstP)
	p.commit("feature.txt", "one\ntwo\nthree\nfour-priv\nfive\nsix\n", "duplicate extra P")
	liveHead := run(t, p.dir, "git", "rev-parse", "HEAD")

	retention, err := ComparePatchRetention(context.Background(), p.dir, liveHead, privateHead)
	if err != nil {
		t.Fatal(err)
	}
	if !retention.RetainsAll() {
		t.Fatalf("ordered unique mapping rejected duplicate extra: %+v", retention)
	}
}

func TestComparePatchRetention_PartialRemovalIsUnretained(t *testing.T) {
	for _, test := range []struct {
		name   string
		remove func(*patchRepo)
	}{
		{
			name: "one of multiple files restored",
			remove: func(p *patchRepo) {
				p.commit("second.txt", "second-base\n", "restore one private path")
			},
		},
		{
			name: "restoration combined with another same-path change",
			remove: func(p *patchRepo) {
				p.commit("feature.txt", "one\ntwo\nthree\nfour\nfive\nsix\ncombined-extra\n", "restore and extend required path")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newPatchRepo(t)
			base := run(t, p.dir, "git", "rev-parse", "HEAD")
			writeFile(t, filepath.Join(p.dir, "second.txt"), "second-base\n")
			run(t, p.dir, "git", "add", ".")
			run(t, p.dir, "git", "commit", "-m", "extend base")
			base = run(t, p.dir, "git", "rev-parse", "HEAD")

			p.checkoutNew("private", base)
			writeFile(t, filepath.Join(p.dir, "feature.txt"), "one\ntwo\nthree\nfour-priv\nfive\nsix\n")
			writeFile(t, filepath.Join(p.dir, "second.txt"), "second-private\n")
			run(t, p.dir, "git", "add", ".")
			run(t, p.dir, "git", "commit", "-m", "private multi-path change")
			privateHead := run(t, p.dir, "git", "rev-parse", "HEAD")

			p.checkoutNew("live", base)
			writeFile(t, filepath.Join(p.dir, "feature.txt"), "one\ntwo\nthree\nfour-priv\nfive\nsix\n")
			writeFile(t, filepath.Join(p.dir, "second.txt"), "second-private\n")
			run(t, p.dir, "git", "add", ".")
			run(t, p.dir, "git", "commit", "-m", "replay private multi-path change")
			test.remove(p)
			liveHead := run(t, p.dir, "git", "rev-parse", "HEAD")

			retention, err := ComparePatchRetention(context.Background(), p.dir, liveHead, privateHead)
			if err != nil {
				t.Fatal(err)
			}
			if retention.RetainsAll() {
				t.Fatalf("partially removed replay read as retained: %+v", retention)
			}
		})
	}
}

func TestComparePatchRetention_ReplayedThenDeletedIsUnretained(t *testing.T) {
	p := newPatchRepo(t)
	base := run(t, p.dir, "git", "rev-parse", "HEAD")
	p.checkoutNew("private", base)
	privateHead := p.commit("feature.txt", "one\ntwo\nthree\nfour-priv\nfive\nsix\n", "private change")

	p.checkoutNew("live", base)
	p.commit("feature.txt", "one\ntwo\nthree\nfour-priv\nfive\nsix\n", "private change replayed")
	run(t, p.dir, "git", "rm", "feature.txt")
	run(t, p.dir, "git", "commit", "-m", "delete replayed file")
	liveHead := run(t, p.dir, "git", "rev-parse", "HEAD")

	retention, err := ComparePatchRetention(context.Background(), p.dir, liveHead, privateHead)
	if err != nil {
		t.Fatal(err)
	}
	if retention.RetainsAll() || len(retention.Unretained) != 1 || retention.Unretained[0] != privateHead {
		t.Fatalf("deleted replay read as retained: %+v", retention)
	}
}

func TestComparePatchRetention_ReplayedThenRevertedIsUnretained(t *testing.T) {
	p := newPatchRepo(t)
	base := run(t, p.dir, "git", "rev-parse", "HEAD")
	p.checkoutNew("private", base)
	privateHead := p.commit("feature.txt", "one\ntwo\nthree\nfour-priv\nfive\nsix\n", "private change")

	p.checkoutNew("live", base)
	replay := p.commit("feature.txt", "one\ntwo\nthree\nfour-priv\nfive\nsix\n", "private change replayed")
	run(t, p.dir, "git", "revert", "--no-edit", replay)
	liveHead := run(t, p.dir, "git", "rev-parse", "HEAD")

	retention, err := ComparePatchRetention(context.Background(), p.dir, liveHead, privateHead)
	if err != nil {
		t.Fatal(err)
	}
	if retention.RetainsAll() || len(retention.Unretained) != 1 || retention.Unretained[0] != privateHead {
		t.Fatalf("reverted replay read as retained: %+v", retention)
	}
}

// TestComparePatchRetention_ConflictResolvedReplayStaysUnretained: a rebase
// that resolved a conflict rewrote the patch itself, so patch identity cannot
// tell it from a genuine loss - it must stay unretained (the run-owned
// exception, not content guessing, is what reconciles those).
func TestComparePatchRetention_ConflictResolvedReplayStaysUnretained(t *testing.T) {
	p := newPatchRepo(t)
	base := run(t, p.dir, "git", "rev-parse", "HEAD")

	p.checkoutNew("private", base)
	privateHead := p.commit("feature.txt", "one\ntwo\nthree\nfour-priv\nfive\nsix\n", "private change")

	p.checkoutNew("live", base)
	p.commit("feature.txt", "one\ntwo\nthree\nfour-live\nfive\nsix\n", "live-side change to the same line")
	// Replay conflicts on the shared line; keep both sides' intent.
	writeFile(t, filepath.Join(p.dir, "feature.txt"), "one\ntwo\nthree\nfour-priv-live\nfive\nsix\n")
	run(t, p.dir, "git", "add", ".")
	run(t, p.dir, "git", "commit", "-m", "private change conflict-resolved")
	liveHead := run(t, p.dir, "git", "rev-parse", "HEAD")

	retention, err := ComparePatchRetention(context.Background(), p.dir, liveHead, privateHead)
	if err != nil {
		t.Fatal(err)
	}
	if retention.RetainsAll() {
		t.Fatalf("conflict-resolved replay read as retained: %+v", retention)
	}
	if !retention.Comparable || len(retention.Unretained) != 1 || retention.Unretained[0] != privateHead {
		t.Fatalf("conflict-resolved replay not reported unretained: %+v", retention)
	}
}

// TestComparePatchRetention_ContainedHistoryNeedsNoPatch: an ancestor's
// changes are retained by object identity, so the exclusive range is empty and
// the comparison is trivially complete.
func TestComparePatchRetention_ContainedHistoryNeedsNoPatch(t *testing.T) {
	p := newPatchRepo(t)
	base := run(t, p.dir, "git", "rev-parse", "HEAD")
	p.checkoutNew("private", base)
	privateHead := p.commit("feature.txt", "one\ntwo\nthree\nfour-priv\nfive\nsix\n", "private change")
	p.commit("extra.txt", "extra\n", "descendant")
	liveHead := run(t, p.dir, "git", "rev-parse", "HEAD")

	retention, err := ComparePatchRetention(context.Background(), p.dir, liveHead, privateHead)
	if err != nil {
		t.Fatal(err)
	}
	if !retention.RetainsAll() || len(retention.Retained) != 0 {
		t.Fatalf("contained history not trivially retained: %+v", retention)
	}
}
