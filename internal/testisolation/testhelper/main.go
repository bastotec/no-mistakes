//go:build linux

// A non-race helper keeps process-boundary tests fast under go test -race.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kunchenguid/no-mistakes/internal/testisolation"
	"github.com/kunchenguid/no-mistakes/internal/testisolation/testfixture"
)

type request struct {
	Canary, HiddenFile string
	Heartbeat          string
	Mode               string
	Hold               bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) > 2 && os.Args[1] == "heartbeat" {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if err := os.WriteFile(os.Args[2], []byte(time.Now().String()), 0600); err != nil {
				return err
			}
			time.Sleep(30 * time.Millisecond)
		}
		return fmt.Errorf("heartbeat expired")
	}
	if len(os.Args) > 1 && os.Args[1] == "grandchild" {
		var r request
		if err := json.NewDecoder(os.Stdin).Decode(&r); err != nil {
			return err
		}
		return probe(r)
	}
	if err := testisolation.RestrictWorkerNetwork(); err != nil {
		return err
	}
	var r request
	if err := json.NewDecoder(os.Stdin).Decode(&r); err != nil {
		return err
	}
	if r.Mode == "postgres" {
		closeServices, err := testisolation.ServePrivateServices()
		if err != nil {
			return err
		}
		defer closeServices()
	} else {
		closeDatabase, err := testfixture.Start()
		if err != nil {
			return err
		}
		defer closeDatabase()
		closeModel, err := testisolation.ServeModelFrontend()
		if err != nil {
			return err
		}
		defer closeModel()
	}
	if r.Mode == "native-deck" {
		if err := os.WriteFile("/tmp/model-client-key", []byte("isolated-client"), 0600); err != nil {
			return err
		}
		cmd := exec.Command("/nm/deck", "run", "--ephemeral", "--no-project-context", "--base-url", testisolation.ModelURL, "--model", "fixture-model", "--api-key-file", "/tmp/model-client-key", "--max-turns", "1", "--deadline-secs", "10", "--provider-retry-secs", "0", "-")
		cmd.Stdin = strings.NewReader("Return the channel probe result without tools.")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	if r.Heartbeat != "" {
		cmd := exec.Command("/nm/worker", "heartbeat", r.Heartbeat)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		deadline := time.Now().Add(3 * time.Second)
		for {
			if _, err := os.Stat(r.Heartbeat); err == nil {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("heartbeat never started")
			}
			time.Sleep(10 * time.Millisecond)
		}
		fmt.Println("ready")
		if r.Mode == "failure" {
			return fmt.Errorf("injected worker failure")
		}
		if !r.Hold {
			return nil
		}
	}
	if r.Hold {
		select {}
	}
	cmd := exec.Command("/nm/worker", "grandchild")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	input, _ := json.Marshal(r)
	cmd.Stdin = strings.NewReader(string(input))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
func probe(r request) error {
	want, actual := testisolation.Environment(os.Getenv("PWD")), os.Environ()
	sort.Strings(want)
	sort.Strings(actual)
	if !reflect.DeepEqual(want, actual) {
		return fmt.Errorf("descendant environment differs from exact allowlist: %v", actual)
	}
	for _, key := range []string{"AWS_SECRET_ACCESS_KEY", "GH_TOKEN", "UNRECOGNIZED_CREDENTIAL", "LD_PRELOAD", "HTTP_PROXY", "SSH_AUTH_SOCK", "NM_HOME", "NM_TEST_MODEL_KEY_FILE"} {
		if _, ok := os.LookupEnv(key); ok {
			return fmt.Errorf("ambient variable leaked: %s", key)
		}
	}
	if _, err := os.Stat(r.HiddenFile); !os.IsNotExist(err) {
		return fmt.Errorf("host file exposed: %v", err)
	}
	if os.Getenv("NM_TEST_DATABASE_URL") != testisolation.DatabaseURL {
		return fmt.Errorf("database URL not fixed")
	}
	// Namespace isolation alone would still allow arbitrary namespace-local
	// listeners. Landlock must deny every TCP port except DB/model, and the
	// inherited seccomp filter must close non-TCP and Unix-path bypasses.
	if _, _, errno := syscall.RawSyscall(unix.SYS_IO_URING_SETUP, 0, 0, 0); errno != syscall.EPERM {
		return fmt.Errorf("io_uring socket bypass not denied: %v", errno)
	}
	if listener, err := net.Listen("tcp4", "127.0.0.1:17683"); err == nil {
		listener.Close()
		return fmt.Errorf("unlisted loopback TCP bind was not denied")
	}
	for _, domainTypeProtocol := range [][3]int{
		{syscall.AF_INET, syscall.SOCK_DGRAM, 0},
		{syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_ICMP},
		{syscall.AF_UNIX, syscall.SOCK_STREAM, 0},
		{syscall.AF_INET, syscall.SOCK_STREAM, 262}, // MPTCP bypasses Landlock TCP rules.
	} {
		fd, err := syscall.Socket(domainTypeProtocol[0], domainTypeProtocol[1], domainTypeProtocol[2])
		if err != syscall.EPERM {
			if err == nil {
				syscall.Close(fd)
			}
			return fmt.Errorf("socket family/type/protocol %v not denied by policy: %v", domainTypeProtocol, err)
		}
	}
	if fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0); err != syscall.EPERM {
		if err == nil {
			syscall.Close(fds[0])
			syscall.Close(fds[1])
		}
		return fmt.Errorf("Unix datagram socketpair not denied: %v", err)
	}
	if fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0); err != nil {
		return fmt.Errorf("private stream socketpair unexpectedly denied: %v", err)
	} else {
		syscall.Close(fds[0])
		syscall.Close(fds[1])
	}
	client := &http.Client{Timeout: 3 * time.Second}
	if r.Mode == "postgres" {
		psql := exec.Command("/nm/postgres/bin/psql", testisolation.DatabaseURL, "-X", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-c", "CREATE TABLE item(value TEXT); BEGIN; INSERT INTO item VALUES ('committed'); COMMIT; SELECT value FROM item")
		psql.Env = append(os.Environ(), "LD_LIBRARY_PATH=/nm/postgres/lib")
		output, err := psql.CombinedOutput()
		if err != nil || !strings.Contains(string(output), "committed") {
			return fmt.Errorf("native PostgreSQL transaction: %v: %s", err, output)
		}
	} else {
		body := `[{"sql":"CREATE TABLE item(value TEXT)"},{"sql":"INSERT INTO item VALUES (?)","args":["committed"]},{"sql":"SELECT value FROM item"}]`
		response, err := client.Post(testfixture.DatabaseURL, "application/json", strings.NewReader(body))
		if err != nil {
			return fmt.Errorf("fixture database TCP transaction: %w", err)
		}
		output, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 || !strings.Contains(string(output), "committed") {
			return fmt.Errorf("fixture database transaction: %d %s", response.StatusCode, output)
		}
	}
	for _, address := range []string{r.Canary, "203.0.113.1:443", "[2001:db8::1]:443"} {
		conn, err := net.DialTimeout("tcp", address, 300*time.Millisecond)
		if err == nil {
			conn.Close()
			return fmt.Errorf("forbidden network reached: %s", address)
		}
	}
	conn, err := net.DialTimeout("udp", "192.0.2.53:53", 300*time.Millisecond)
	if err == nil {
		_, err = conn.Write([]byte{0, 0, 0, 0})
		conn.Close()
	}
	if err == nil {
		return fmt.Errorf("external DNS datagram was not denied")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if addresses, err := net.DefaultResolver.LookupHost(ctx, "network-canary.invalid"); err == nil {
		return fmt.Errorf("unexpected DNS success: %v", addresses)
	}
	request, err := http.NewRequest(http.MethodPost, testisolation.ModelURL+"/responses", strings.NewReader(`{"model":"fixture-model","stream":true,"messages":[]}`))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer do-not-forward")
	request.Header.Set("Cookie", "host-session=do-not-forward")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("model channel: %w", err)
	}
	output, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(string(output), "model-fixture") {
		return fmt.Errorf("model response: %d %s", response.StatusCode, output)
	}
	response, err = client.Get(testisolation.ModelURL + "/admin")
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		return fmt.Errorf("model admin channel not refused")
	}
	fmt.Println("detached grandchild: database transaction, model streaming, credential and egress boundaries passed")
	return nil
}
