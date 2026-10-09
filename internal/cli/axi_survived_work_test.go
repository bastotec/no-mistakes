package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
	toon "github.com/toon-format/toon-go"
)

func TestAxiSurvivedWorkVisibleForFailedRunAndGate(t *testing.T) {
	raw, err := types.MarshalFindingsJSON(types.Findings{SurvivedWork: &types.SurvivedWork{Worktree: "/run/worktree", Files: []string{" M tracked.txt", "?? new.txt"}, Next: "next repair preserves edits"}})
	if err != nil {
		t.Fatal(err)
	}
	step := stepView{Name: "test", Status: "failed", FindingsJSON: raw}
	for _, obj := range []any{toon.NewObject(runObjectField(runView{ID: "run1", Status: "failed", Steps: []stepView{step}})), toon.NewObject(gateFieldsWithHelp(step, nil)...)} {
		data, err := toon.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"survived_work", "/run/worktree", "tracked.txt", "next repair preserves edits"} {
			if !strings.Contains(string(data), want) {
				t.Fatalf("missing %s: %s", want, data)
			}
		}
	}
}
