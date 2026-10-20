package testisolation

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

var helperOnce sync.Once
var helperPath string
var helperError error

func buildHelper(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux confinement only")
	}
	if err := Check(); err != nil {
		t.Skip(err)
	}
	helperOnce.Do(func() {
		root, err := os.MkdirTemp("", "nm-isolation-helper-")
		if err != nil {
			helperError = err
			return
		}
		helperPath = filepath.Join(root, "worker")
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", "build", "-o", helperPath, "./testhelper")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if output, err := cmd.CombinedOutput(); err != nil {
			helperError = &buildError{err, string(output)}
		}
	})
	if helperError != nil {
		t.Fatal(helperError)
	}
	return helperPath
}

type buildError struct {
	err    error
	output string
}

func (e *buildError) Error() string { return e.err.Error() + ": " + e.output }

func TestMain(m *testing.M) {
	os.Unsetenv("GIT_CONFIG_COUNT")
	code := m.Run()
	if helperPath != "" {
		os.RemoveAll(filepath.Dir(helperPath))
	}
	os.Exit(code)
}

func TestNamespaceDetachedDescendantBoundaries(t *testing.T) {
	runNamespaceBoundaryProbe(t, "")
}

func TestNamespacePostgresDetachedDescendantBoundaries(t *testing.T) {
	if os.Getenv("NM_TEST_POSTGRES_ROOT") == "" {
		t.Skip("set NM_TEST_POSTGRES_ROOT for native PostgreSQL boundary validation")
	}
	root, err := PostgresRoot()
	if err != nil {
		t.Fatal(err)
	}
	runNamespaceBoundaryProbe(t, root)
}

func runNamespaceBoundaryProbe(t *testing.T, postgresRoot string) {
	t.Helper()
	worker := buildHelper(t)
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	hidden := filepath.Join(root, "host-secret")
	os.WriteFile(hidden, []byte("host-only"), 0600)
	key := filepath.Join(root, "key")
	os.WriteFile(key, []byte("fixture-key"), 0600)
	state := filepath.Join(root, "state")
	os.WriteFile(state, []byte("fixture-state"), 0600)
	canary, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer canary.Close()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-key" || r.Header.Get("Cookie") != "" || r.URL.Path != "/v1/responses" {
			t.Errorf("model authority was not pinned: %s %v", r.URL.Path, r.Header)
			http.Error(w, "bad", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: model-fixture\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer model.Close()
	for _, key := range []string{"AWS_SECRET_ACCESS_KEY", "GH_TOKEN", "UNRECOGNIZED_CREDENTIAL", "HTTP_PROXY", "SSH_AUTH_SOCK", "NM_HOME"} {
		t.Setenv(key, "ambient-sentinel")
	}
	t.Setenv("NM_TEST_MODEL_KEY_FILE", key)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	invocation, err := Start(ctx, Options{Worker: worker, Deck: worker, State: state, WorkDir: work, ScratchParent: root, PostgresRoot: postgresRoot, Model: ModelConfig{URL: model.URL + "/v1", KeyFile: key, Model: "fixture-model"}})
	if err != nil {
		t.Fatal(err)
	}
	scratch := invocation.root
	mode := ""
	if postgresRoot != "" {
		mode = "postgres"
	}
	invocation.Command.Stdin = strings.NewReader(`{"Canary":"` + canary.Addr().String() + `","HiddenFile":"` + hidden + `","Mode":"` + mode + `"}`)
	output, err := shellenv.CombinedOutputShellCommand(invocation.Command)
	invocation.Close()
	if err != nil {
		t.Fatalf("boundary helper: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte("detached grandchild:")) {
		t.Fatalf("missing live proof: %s", output)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("scratch survived: %v", err)
	}
	if os.Getenv("GH_TOKEN") != "ambient-sentinel" {
		t.Fatal("daemon environment changed")
	}
	conn, err := net.DialTimeout("tcp", canary.Addr().String(), time.Second)
	if err != nil {
		t.Fatal("host networking changed:", err)
	}
	conn.Close()
}

func TestEnvironmentStartsEmpty(t *testing.T) {
	t.Setenv("UNRECOGNIZED_CREDENTIAL", "sentinel")
	env := Environment("/work")
	for _, entry := range env {
		if strings.Contains(entry, "sentinel") || strings.HasPrefix(entry, "UNRECOGNIZED_CREDENTIAL=") {
			t.Fatal("ambient credential leaked")
		}
	}
	if len(env) != 15 {
		t.Fatalf("allowlist changed: %v", env)
	}
}

func TestNamespaceReapsDetachedChildrenOnEveryExit(t *testing.T) {
	worker := buildHelper(t)
	for _, mode := range []string{"success", "failure", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			work := filepath.Join(root, "work")
			os.Mkdir(work, 0700)
			key := filepath.Join(root, "key")
			os.WriteFile(key, []byte("fixture-key"), 0600)
			state := filepath.Join(root, "state")
			os.WriteFile(state, []byte("fixture-state"), 0600)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			invocation, err := Start(ctx, Options{Worker: worker, Deck: worker, State: state, WorkDir: work, ScratchParent: root, Model: ModelConfig{URL: "http://127.0.0.1:1/v1", KeyFile: key, Model: "fixture-model"}})
			if err != nil {
				t.Fatal(err)
			}
			defer invocation.Close()
			heartbeat := filepath.Join(work, "heartbeat")
			invocation.Command.Stdin = strings.NewReader(`{"Heartbeat":"` + heartbeat + `","Mode":"` + mode + `","Hold":` + fmt.Sprint(mode == "cancel") + `}`)
			out, err := invocation.Command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			invocation.Command.Stderr = &stderr
			if err := shellenv.StartShellCommand(invocation.Command); err != nil {
				t.Fatal(err)
			}
			defer shellenv.TerminateShellCommandGroup(invocation.Command)
			ready := make(chan string, 1)
			go func() { line, _ := bufio.NewReader(out).ReadString('\n'); ready <- line }()
			select {
			case line := <-ready:
				if line != "ready\n" {
					t.Fatalf("helper did not reach ready: %q %s", line, stderr.String())
				}
			case <-ctx.Done():
				t.Fatal("helper did not start")
			}
			if mode == "cancel" {
				cancel()
			}
			err = invocation.Command.Wait()
			if mode == "success" && err != nil {
				t.Fatalf("helper: %v %s", err, stderr.String())
			}
			if mode != "success" && err == nil {
				t.Fatal("expected interrupted/failed helper")
			}
			first, err := os.ReadFile(heartbeat)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(200 * time.Millisecond)
			after, err := os.ReadFile(heartbeat)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first, after) {
				t.Fatal("detached grandchild survived namespace teardown")
			}
			invocation.Close()
			if _, err := os.Stat(invocation.root); !os.IsNotExist(err) {
				t.Fatalf("invocation scratch remains: %v", err)
			}
		})
	}
}

// Optional native-client integration: the model is still a deterministic local
// SSE fixture, never a provider. Enable with NM_TEST_REAL_DECK_BINARY=/path/deck.
func TestNamespaceNativeDeckModelTransport(t *testing.T) {
	deck := os.Getenv("NM_TEST_REAL_DECK_BINARY")
	if deck == "" {
		t.Skip("set NM_TEST_REAL_DECK_BINARY for the installed-client integration")
	}
	worker := buildHelper(t)
	root := t.TempDir()
	work := filepath.Join(root, "work")
	os.Mkdir(work, 0700)
	key := filepath.Join(root, "key")
	os.WriteFile(key, []byte("fixture-key"), 0600)
	state := filepath.Join(root, "state")
	os.WriteFile(state, []byte("fixture-state"), 0600)
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer fixture-key" {
			t.Errorf("host-side model credential not applied")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"delta\":\"channel-proven\",\"item_id\":\"message\",\"output_index\":0,\"content_index\":0}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"fixture\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"model\":\"fixture-model\",\"tools\":[],\"output\":[{\"type\":\"message\",\"id\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"channel-proven\",\"annotations\":[]}]}]}}\n\n")
	}))
	defer model.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	invocation, err := Start(ctx, Options{Worker: worker, Deck: deck, State: state, WorkDir: work, ScratchParent: root, Model: ModelConfig{URL: model.URL + "/v1", KeyFile: key, Model: "fixture-model"}})
	if err != nil {
		t.Fatal(err)
	}
	defer invocation.Close()
	invocation.Command.Stdin = strings.NewReader(`{"Mode":"native-deck"}`)
	output, err := shellenv.CombinedOutputShellCommand(invocation.Command)
	if err != nil {
		t.Fatalf("native Deck: %v\n%s", err, output)
	}
	if calls.Load() != 1 || !bytes.Contains(output, []byte(`"type":"run_finished"`)) || !bytes.Contains(output, []byte("channel-proven")) {
		t.Fatalf("native model channel was not proven: calls=%d\n%s", calls.Load(), output)
	}
}
