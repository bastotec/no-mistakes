package testisolation

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// DatabaseURL is a standard PostgreSQL DSN, including for original Prisma
// clients. No host/caller database URL is accepted or forwarded.
const DatabaseURL = "postgresql://nm_test@127.0.0.1:17681/nm_test?sslmode=disable"
const ModelURL = "http://127.0.0.1:17682/v1"

// PostgresRoot is a user-supplied native PostgreSQL installation prefix. It is
// mounted read-only; initdb data and every database process live only inside
// this invocation's namespaces. Distribution packaging may use an internal bin
// symlink to its versioned bin directory, preserving native path relocation.
func PostgresRoot() (string, error) {
	root := os.Getenv("NM_TEST_POSTGRES_ROOT")
	if root == "" || !filepath.IsAbs(root) {
		return "", fmt.Errorf("test-isolation-postgres: NM_TEST_POSTGRES_ROOT must name an absolute native PostgreSQL installation prefix")
	}
	for _, binary := range []string{"initdb", "postgres", "psql"} {
		info, err := os.Stat(filepath.Join(root, "bin", binary))
		if err != nil || info.IsDir() || info.Mode()&0111 == 0 {
			return "", fmt.Errorf("test-isolation-postgres: installation must provide executable bin/%s", binary)
		}
	}
	if os.Getuid() == 0 {
		return "", fmt.Errorf("test-isolation-postgres: PostgreSQL requires an unprivileged daemon UID")
	}
	return root, nil
}

func postgresEnvironment() []string {
	return append(Environment(os.Getenv("PWD")), "LD_LIBRARY_PATH=/nm/postgres/lib")
}

// startPostgres is called only inside the already-created worker namespace.
// There is no host listener, container daemon, port forwarding, remote URL, or
// reused cluster. Even SIGKILL discards the tmpfs data when the namespace dies.
func startPostgres() (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	initdb := exec.CommandContext(ctx, "/nm/postgres/bin/initdb", "-D", "/tmp/pgdata", "-U", "nm_test", "--auth-local=trust", "--auth-host=trust", "--locale=C", "--encoding=UTF8", "--no-sync")
	initdb.Env = postgresEnvironment()
	shellenv.ConfigureShellCommand(initdb)
	output, err := shellenv.CombinedOutputShellCommand(initdb)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("test-isolation-postgres: initdb failed: %w: %s", err, output)
	}
	log, err := os.Create("/tmp/postgres.log")
	if err != nil {
		return nil, err
	}
	serverCtx, stop := context.WithCancel(context.Background())
	server := exec.CommandContext(serverCtx, "/nm/postgres/bin/postgres", "-D", "/tmp/pgdata", "-h", "127.0.0.1", "-p", "17681", "-c", "unix_socket_directories=", "-c", "shared_buffers=8MB", "-c", "max_connections=20", "-c", "fsync=off")
	server.Env = postgresEnvironment()
	server.Stdout = log
	server.Stderr = log
	shellenv.ConfigureShellCommand(server)
	if err := shellenv.StartShellCommand(server); err != nil {
		stop()
		log.Close()
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- server.Wait() }()
	cleanup := func() { stop(); <-done; shellenv.TerminateShellCommandGroup(server); log.Close() }
	deadline := time.Now().Add(10 * time.Second)
	for {
		connection, err := net.DialTimeout("tcp4", "127.0.0.1:17681", 100*time.Millisecond)
		if err == nil {
			connection.Close()
			break
		}
		select {
		case err := <-done:
			stop()
			log.Close()
			details, _ := os.ReadFile("/tmp/postgres.log")
			return nil, fmt.Errorf("test-isolation-postgres: server failed: %v: %s", err, details)
		default:
		}
		if time.Now().After(deadline) {
			cleanup()
			return nil, fmt.Errorf("test-isolation-postgres: readiness deadline exceeded")
		}
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	create := exec.CommandContext(ctx, "/nm/postgres/bin/psql", "postgresql://nm_test@127.0.0.1:17681/postgres?sslmode=disable", "-X", "-v", "ON_ERROR_STOP=1", "-c", "CREATE DATABASE nm_test")
	create.Env = postgresEnvironment()
	shellenv.ConfigureShellCommand(create)
	if output, err := shellenv.CombinedOutputShellCommand(create); err != nil {
		cleanup()
		return nil, fmt.Errorf("test-isolation-postgres: create disposable database: %w: %s", err, output)
	}
	return cleanup, nil
}
