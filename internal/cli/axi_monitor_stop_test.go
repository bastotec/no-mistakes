package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestAxiMonitorStopRequiresExactIdentity(t *testing.T) {
	out, err := executeCmd("axi", "monitor-stop")
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("missing supported exact-identity operation: err=%v output=%s", err, out)
	}
}

func TestAxiMonitorStopRPCAndOldDaemonRefusal(t *testing.T) {
	for _, supported := range []bool{false, true} {
		t.Run(fmt.Sprint(supported), func(t *testing.T) {
			root := makeSocketSafeTempDir(t)
			t.Setenv("NM_HOME", root)
			p := paths.WithRoot(root)
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			srv := ipc.NewServer()
			pin := db.MonitorDisposition{RunID: "exact-run", RepoID: "exact-repo", PRURL: "https://github.com/example/repo/pull/1", PublishedHead: strings.Repeat("a", 40), ObservedHead: strings.Repeat("b", 40), State: "parked-review"}
			if supported {
				srv.Handle(ipc.MethodStopMonitor, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
					var got db.MonitorDisposition
					if err := json.Unmarshal(raw, &got); err != nil {
						return nil, err
					}
					if got != pin {
						return nil, fmt.Errorf("identity lost: %+v", got)
					}
					return &ipc.StopMonitorResult{MonitorStopped: true}, nil
				})
			}
			if err := srv.Listen(p.Socket()); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- srv.ServeReady() }()
			t.Cleanup(func() {
				srv.Close()
				select {
				case <-exited:
				case <-time.After(time.Second):
					t.Error("test server outlived owner")
				}
			})
			out, err := executeCmd("axi", "monitor-stop", "--run", pin.RunID, "--repo-id", pin.RepoID, "--pr-url", pin.PRURL, "--published-head", pin.PublishedHead, "--observed-head", pin.ObservedHead, "--state", pin.State)
			if supported {
				if err != nil || !strings.Contains(out, "monitor_stopped: true") || !strings.Contains(out, "custody_returned: false") {
					t.Fatalf("supported response: %s %v", out, err)
				}
			} else if err == nil || strings.Contains(out, "monitor_stopped: true") {
				t.Fatalf("old daemon falsely succeeded: %s %v", out, err)
			}
		})
	}
}
