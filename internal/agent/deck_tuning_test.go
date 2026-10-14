package agent

import (
	"reflect"
	"testing"
)

func TestDeckBuildArgsAppliesInvocationTuningOverOperatorFlags(t *testing.T) {
	a := &deckAgent{extraArgs: []string{"--effort", "high", "--max-turns", "400", "--deadline-secs=1800", "--api-key-file", "/k", "--model", "chain/review"}}

	got := a.buildArgs(&InvocationTuning{Model: "zai/glm-5.3", Effort: "low", MaxTurns: 60, DeadlineSecs: 600})
	want := []string{"run", "--api-key-file", "/k", "--model", "zai/glm-5.3", "--effort", "low", "--max-turns", "60", "--deadline-secs", "600", "--ephemeral", "-"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %q\nwant %q", got, want)
	}

	if got := a.buildArgs(nil); !reflect.DeepEqual(got, append(append([]string{"run"}, a.extraArgs...), "--ephemeral", "-")) {
		t.Fatalf("untuned args changed: %q", got)
	}
	if got := a.buildArgs(&InvocationTuning{Effort: "medium"}); !reflect.DeepEqual(got, []string{"run", "--max-turns", "400", "--deadline-secs=1800", "--api-key-file", "/k", "--model", "chain/review", "--effort", "medium", "--ephemeral", "-"}) {
		t.Fatalf("partial tuning must keep the other operator flags: %q", got)
	}
	if got := (&deckAgent{extraArgs: []string{"-m", "x"}}).buildArgs(&InvocationTuning{Model: "y"}); !reflect.DeepEqual(got, []string{"run", "--model", "y", "--ephemeral", "-"}) {
		t.Fatalf("short model flag must be replaced: %q", got)
	}
	medium := &deckAgent{extraArgs: []string{"--model", "operator/model", "--effort=medium", "--max-turns", "20"}}
	if got := medium.buildArgs(&InvocationTuning{MaxTurns: 400, DeadlineSecs: 1800}); !reflect.DeepEqual(got, []string{"run", "--model", "operator/model", "--effort=medium", "--max-turns", "400", "--deadline-secs", "1800", "--ephemeral", "-"}) {
		t.Fatalf("bounds-only medium-level tuning must preserve the operator's model and effort: %q", got)
	}
}

func TestPiBuildArgsAppliesModelAndThinkingTuning(t *testing.T) {
	pa := &piAgent{extraArgs: []string{"--model", "chain/review", "--thinking", "high"}}
	got := pa.buildArgs(nil, &InvocationTuning{Model: "zai/glm-5.3", Effort: "low", MaxTurns: 60})
	want := []string{"--model", "zai/glm-5.3", "--thinking", "low", "--mode", "json", "--no-session"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %q\nwant %q", got, want)
	}
}

func TestClaudeBuildArgsAppliesModelAndEffortTuning(t *testing.T) {
	ca := &claudeAgent{extraArgs: []string{"--model", "opus", "--effort", "high"}}
	got := ca.buildArgs(nil, "", &InvocationTuning{Model: "sonnet", Effort: "low", MaxTurns: 60})
	if !reflect.DeepEqual(got[:4], []string{"--model", "sonnet", "--effort", "low"}) {
		t.Fatalf("args = %q, want the tuned model and effort replacing the operator's", got)
	}
}
