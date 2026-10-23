package citest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
)

// The real CI loop must yield to its owning executor before every poll. This
// guards the integration point independently of the executor handshake tests.
func TestCIStepMonitorCheckpointStopsBeforeNextObservation(t *testing.T) {
	dir, base, head := stepstest.SetupGitRepo(t)
	sctx := stepstest.NewTestContext(t, &stepstest.MockAgent{AgentName: "test"}, dir, base, head, config.Commands{})
	sctx.Env = stepstest.FakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)
	url := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &url
	stopped := errors.New("test owner froze monitor")
	checkpoints := 0
	sctx.MonitorCheckpoint = func(context.Context) error { checkpoints++; return stopped }
	step := (&steps.CIStep{}).SetWaitForNextPoll(func(context.Context, time.Duration) error { t.Fatal("poll continued past frozen boundary"); return nil })
	outcome, err := step.Execute(sctx)
	if !errors.Is(err, stopped) || outcome != nil || checkpoints != 1 {
		t.Fatalf("checkpoint not honored: outcome=%+v err=%v checkpoints=%d", outcome, err, checkpoints)
	}
}
