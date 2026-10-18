package testisolation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPostgresReadinessRequiresSuccessfulQuery(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux PostgreSQL worker")
	}
	root := t.TempDir()
	attempts := filepath.Join(root, "attempts")
	psql := filepath.Join(root, "psql")
	body := `#!/bin/sh
[ "$1" = 'postgresql://nm_test@127.0.0.1:17681/postgres?sslmode=disable' ] || exit 41
[ "$2" = '-X' ] && [ "$3" = '-v' ] && [ "$4" = 'ON_ERROR_STOP=1' ] || exit 42
[ "$5" = '-c' ] && [ "$6" = 'SELECT 1' ] || exit 43
[ "$PGCONNECT_TIMEOUT" = '1' ] || exit 44
count=0
[ ! -f 'ATTEMPTS' ] || read -r count < 'ATTEMPTS'
count=$((count+1))
printf '%s\n' "$count" > 'ATTEMPTS'
if [ "$count" -lt 3 ]; then
 echo 'FATAL: the database system is starting up' >&2
 exit 2
fi
`
	body = strings.ReplaceAll(body, "ATTEMPTS", attempts)
	if err := os.WriteFile(psql, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := waitForPostgres(ctx, psql, make(chan struct{})); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(attempts); err != nil || string(got) != "3\n" {
		t.Fatalf("startup rejection was not retried until query success: %q %v", got, err)
	}
}

func TestPostgresReadinessBoundsFailureAndReapsProbe(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux PostgreSQL worker")
	}
	for _, mode := range []string{"startup-rejected", "probe-hung", "server-exited"} {
		t.Run(mode, func(t *testing.T) {
			body := "#!/bin/sh\nexit 2\n"
			if mode == "probe-hung" {
				body = "#!/bin/sh\nexec /bin/sleep 30\n"
			}
			psql := filepath.Join(t.TempDir(), "psql")
			if err := os.WriteFile(psql, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			if mode == "server-exited" {
				close(done)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			started := time.Now()
			err := waitForPostgres(ctx, psql, done)
			if mode == "server-exited" {
				if err == nil || !strings.Contains(err.Error(), "server exited") {
					t.Fatalf("server exit lost: %v", err)
				}
			} else if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("readiness deadline lost: %v", err)
			}
			if time.Since(started) > 3*time.Second {
				t.Fatal("readiness probe exceeded its deadline")
			}
		})
	}
}
