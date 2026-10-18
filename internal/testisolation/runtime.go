package testisolation

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// Check refuses unsupported confinement; callers must not retry unconfined.
func Check() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("test-isolation-platform: Linux is required")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		return fmt.Errorf("test-isolation-confinement: bubblewrap is required")
	}
	return nil
}

// Options are built by the daemon, never by repository configuration.
type Options struct {
	Worker, Deck, State, WorkDir, GateDir, EvidenceDir, ScratchParent string
	PostgresRoot                                                      string
	Model                                                             ModelConfig
}

type Invocation struct {
	Command        *exec.Cmd
	root           string
	listener       net.Listener
	server         *http.Server
	closeTransport func()
	policyFile     *os.File
	workerChannel  *os.File
}

// Environment is constructed from an empty allowlist. There is deliberately no
// ambient PATH, loader, shell startup, proxy, Git config or credential overlay.
func Environment(workdir string) []string {
	return []string{"PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "HOME=/tmp/home", "TMPDIR=/tmp", "PWD=" + workdir, "NO_MISTAKES_GATE=1", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_EDITOR=true", "GIT_SEQUENCE_EDITOR=true", "DATABASE_URL=" + DatabaseURL, "NM_TEST_DATABASE_URL=" + DatabaseURL, "PROXAI_BASE_URL=" + ModelURL, "DECK_NO_PROJECT_CONTEXT=1"}
}

// Start prepares one invocation. The namespace's PID 1 owns every Test process,
// including detached grandchildren; killing it tears down the whole namespace.
// /tmp is a fresh tmpfs, so database, HOME and model-frontend state disappear on
// worker/daemon death without requiring a host-side database cleanup process.
func Start(ctx context.Context, o Options) (_ *Invocation, err error) {
	if err := Check(); err != nil {
		return nil, err
	}
	credential, err := filepath.EvalSymlinks(o.Model.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("test-isolation-model-credential: configured key file is unavailable")
	}
	for _, mount := range []string{o.WorkDir, o.GateDir, o.EvidenceDir, o.PostgresRoot, o.Worker, o.Deck, o.State, "/usr", "/bin", "/lib", "/lib64"} {
		if mount == "" {
			continue
		}
		canonical, err := filepath.EvalSymlinks(mount)
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(canonical, credential)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("test-isolation-model-credential: key file must remain outside every sandbox mount")
		}
	}
	handler, err := modelHandler(o.Model)
	if err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp(o.ScratchParent, ".test-isolation-")
	if err != nil {
		return nil, err
	}
	invocation := &Invocation{root: root, closeTransport: handler.(*modelBridgeHandler).Close}
	defer func() {
		if err != nil {
			invocation.Close()
		}
	}()
	connection, guest, err := modelChannel()
	if err != nil {
		return nil, err
	}
	invocation.workerChannel = guest
	listener := newChannelListener(connection)
	invocation.listener = listener
	policy, err := socketFilter()
	if err != nil {
		return nil, err
	}
	policyPath := filepath.Join(root, "socket-policy")
	if err := os.WriteFile(policyPath, policy, 0600); err != nil {
		return nil, err
	}
	invocation.policyFile, err = os.Open(policyPath)
	if err != nil {
		return nil, err
	}
	invocation.server = &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second} // Lifetime is invocation-owned, including long tool turns.
	go invocation.server.Serve(listener)
	args := []string{"--unshare-all", "--die-with-parent", "--new-session", "--cap-drop", "ALL", "--clearenv", "--seccomp", "3"}
	for _, path := range []string{"/usr", "/bin", "/lib", "/lib64"} {
		if _, e := os.Stat(path); e == nil {
			args = append(args, "--ro-bind", path, path)
		}
	}
	// Public trust roots, not credentials. Native Deck initializes its TLS
	// client even when its only reachable endpoint is the local HTTP bridge.
	const certificates = "/etc/ssl/certs/ca-certificates.crt"
	if _, err := os.Stat(certificates); err == nil {
		args = append(args, "--ro-bind", certificates, certificates)
	}
	args = append(args, "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--dir", "/tmp/home", "--tmpfs", "/nm")
	// Only run-owned paths are writable. Never bind /, HOME, NM_HOME or /run.
	for _, path := range []string{o.WorkDir, o.GateDir, o.EvidenceDir} {
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
			return nil, fmt.Errorf("test-isolation-mount: absolute run-owned paths are required")
		}
		args = append(args, "--bind", path, path)
	}
	for _, binding := range [][2]string{{o.Worker, "/nm/worker"}, {o.Deck, "/nm/deck"}, {o.State, "/nm/state.db"}} {
		args = append(args, "--ro-bind", binding[0], binding[1])
	}
	// Native initdb needs an NSS identity. Supply only this unprivileged
	// namespace identity, not the host's passwd/group databases or HOME.
	passwd := filepath.Join(root, "passwd")
	group := filepath.Join(root, "group")
	if err := os.WriteFile(passwd, []byte(fmt.Sprintf("nm_test:x:%d:%d:isolated Test:/tmp/home:/bin/sh\n", os.Getuid(), os.Getgid())), 0600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(group, []byte(fmt.Sprintf("nm_test:x:%d:\n", os.Getgid())), 0600); err != nil {
		return nil, err
	}
	args = append(args, "--ro-bind", passwd, "/etc/passwd", "--ro-bind", group, "/etc/group")
	if o.PostgresRoot != "" {
		args = append(args, "--ro-bind", o.PostgresRoot, "/nm/postgres")
	}
	for _, entry := range Environment(o.WorkDir) {
		for i := range entry {
			if entry[i] == '=' {
				args = append(args, "--setenv", entry[:i], entry[i+1:])
				break
			}
		}
	}
	args = append(args, "--chdir", o.WorkDir, "/nm/worker", "isolated-test-worker")
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, err
	}
	invocation.Command = exec.CommandContext(ctx, bwrap, args...)
	invocation.Command.Env = []string{"PATH=/usr/bin:/bin"}
	invocation.Command.ExtraFiles = []*os.File{invocation.policyFile, invocation.workerChannel}
	shellenv.ConfigureShellCommand(invocation.Command)
	return invocation, nil
}
func (i *Invocation) Close() {
	if i == nil {
		return
	}
	if i.server != nil {
		i.server.Close()
	}
	if i.listener != nil {
		i.listener.Close()
	}
	if i.closeTransport != nil {
		i.closeTransport()
	}
	if i.policyFile != nil {
		i.policyFile.Close()
	}
	if i.workerChannel != nil {
		i.workerChannel.Close()
	}
	if i.root != "" {
		os.RemoveAll(i.root)
	}
}

// ServePrivateServices must be called only inside the already-created worker
// namespace. Native PostgreSQL is the production database; there is no SQLite
// or network-service fallback if its binaries, libraries or startup fail.
func ServePrivateServices() (func(), error) {
	closeDatabase, err := startPostgres()
	if err != nil {
		return nil, err
	}
	closeModel, err := ServeModelFrontend()
	if err != nil {
		closeDatabase()
		return nil, err
	}
	return func() { closeModel(); closeDatabase() }, nil
}

// ServeModelFrontend also lets confinement fixtures exercise this exact model
// transport independently of the native PostgreSQL provisioning prerequisite.
func ServeModelFrontend() (func(), error) {
	model, err := net.Listen("tcp4", "127.0.0.1:17682")
	if err != nil {
		return nil, err
	}
	file := os.NewFile(4, "model-channel")
	connection, err := net.FileConn(file)
	file.Close()
	if err != nil {
		model.Close()
		return nil, fmt.Errorf("test-isolation-model-channel: inherited stream unavailable: %w", err)
	}
	server := &http.Server{Handler: ModelFrontend(connection), ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(model)
	return func() { server.Close(); connection.Close() }, nil
}
