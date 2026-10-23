package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/spf13/cobra"
)

func TestAxiDocumentTimeoutRefusesOldDaemonWithoutFallback(t *testing.T) {
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{})
	ordinary := false
	fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) { ordinary = true; return nil, nil })
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := runAxiDocumentTimeout(cmd, false, "original task", "", time.Minute, 90*time.Minute)
	if err == nil || !strings.Contains(out.String(), "start_document_run") || !strings.Contains(out.String(), "no ordinary fallback") {
		t.Fatalf("%v\n%s", err, &out)
	}
	if ordinary {
		t.Fatal("old daemon fell back to ordinary launch")
	}
	if got := cliGit(t, ".", "rev-parse", "HEAD"); got != fx.head {
		t.Fatal("caller head moved")
	}
}
