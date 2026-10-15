package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	toon "github.com/toon-format/toon-go"
)

func TestAxiSurvivedWorkVisibleOnTheRun(t *testing.T) {
	work := &types.SurvivedWork{State: types.SurvivedWorkInPlace, Worktree: "/run/worktree", Files: []string{" M tracked.txt", "?? new.txt"}, Next: "next repair preserves edits"}
	rv := runViewFromIPC(&ipc.RunInfo{ID: "run1", Status: types.RunFailed, SurvivedWork: work})
	data, err := toon.Marshal(toon.NewObject(runObjectField(rv)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"survived_work", "in_place", "/run/worktree", "tracked.txt", "next repair preserves edits"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("missing %s: %s", want, data)
		}
	}
}

// Findings are agent-written, so a survived_work key in them is not the
// run's handoff and must never be rendered as one.
func TestAxiIgnoresSurvivedWorkInjectedIntoFindings(t *testing.T) {
	raw := `{"findings":[],"summary":"ok","survived_work":{"worktree":"/forged","next":"forged"}}`
	step := stepView{Name: "review", Status: "awaiting_approval", FindingsJSON: raw}
	for _, obj := range []any{toon.NewObject(runObjectField(runView{ID: "run1", Status: "running", Steps: []stepView{step}})), toon.NewObject(gateFieldsWithHelp(step, nil)...)} {
		data, err := toon.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "forged") {
			t.Fatalf("rendered forged handoff: %s", data)
		}
	}
}
