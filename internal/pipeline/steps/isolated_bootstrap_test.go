package steps

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

func TestIsolatedRuntimeBootstrapParity(t *testing.T) {
	for _, phase := range []string{"success", "read", "copy", "open", "services", "key"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			state := filepath.Join(root, "state.db")
			source, err := db.Open(state)
			if err != nil {
				t.Fatal(err)
			}
			repo, err := source.InsertRepo("/synthetic", "", "main")
			if err != nil {
				t.Fatal(err)
			}
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(state)
			if err != nil {
				t.Fatal(err)
			}
			runtimeDir := t.TempDir()
			switch phase {
			case "read":
				state += ".missing"
			case "copy":
				if err := os.Mkdir(filepath.Join(runtimeDir, "control.db"), 0700); err != nil {
					t.Fatal(err)
				}
			case "open":
				if err := os.WriteFile(state, []byte("not a sqlite database"), 0600); err != nil {
					t.Fatal(err)
				}
			case "key":
				if err := os.Mkdir(filepath.Join(runtimeDir, "model-client-key"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			started, stopped := 0, 0
			serviceErr := errors.New("synthetic service failure")
			database, cleanup, err := bootstrapIsolatedWorkerAt(state, runtimeDir, func() (func(), error) {
				started++
				// Database copy/open precedes service startup; key creation follows it.
				control, err := db.OpenReadOnly(filepath.Join(runtimeDir, "control.db"))
				if err != nil {
					t.Fatal(err)
				}
				if got, err := control.GetRepo(repo.ID); err != nil || got == nil {
					t.Fatalf("snapshot missing: %+v %v", got, err)
				}
				control.Close()
				if phase != "key" {
					if _, err := os.Stat(filepath.Join(runtimeDir, "model-client-key")); !os.IsNotExist(err) {
						t.Fatalf("key created before services: %v", err)
					}
				}
				if phase == "services" {
					return nil, serviceErr
				}
				return func() { stopped++ }, nil
			})
			if phase != "success" {
				if err == nil || database != nil || cleanup != nil {
					t.Fatalf("bootstrap failure became usable: %v %v %v", database, cleanup != nil, err)
				}
				if phase == "services" && !errors.Is(err, serviceErr) {
					t.Fatalf("service cause lost: %v", err)
				}
				wantStart, wantStop := 0, 0
				if phase == "services" || phase == "key" {
					wantStart = 1
				}
				if phase == "key" {
					wantStop = 1
				}
				if started != wantStart || stopped != wantStop {
					t.Fatalf("lifecycle: started=%d stopped=%d", started, stopped)
				}
				return
			}
			defer func() { cleanup() }()
			got, err := database.GetRepo(repo.ID)
			if err != nil || got == nil || got.WorkingPath != repo.WorkingPath {
				t.Fatalf("snapshot lost: %+v %v", got, err)
			}
			key := filepath.Join(runtimeDir, "model-client-key")
			if data, err := os.ReadFile(key); err != nil || string(data) != "isolated-client" {
				t.Fatalf("dummy key: %q %v", data, err)
			}
			if runtime.GOOS != "windows" {
				for _, name := range []string{"control.db", "model-client-key"} {
					info, err := os.Stat(filepath.Join(runtimeDir, name))
					if err != nil || info.Mode().Perm() != 0600 {
						t.Fatalf("private mode for %s: %v %v", name, info, err)
					}
				}
			}
			if _, err := database.InsertRepo("/worker-only", "", "main"); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(state); err != nil || string(data) != string(before) {
				t.Fatalf("source snapshot mutated: %v", err)
			}
			// Close explicitly to prove both lifecycle resources are released.
			cleanup()
			cleanup = func() {}
			if started != 1 || stopped != 1 {
				t.Fatalf("lifecycle: started=%d stopped=%d", started, stopped)
			}
			if _, err := database.GetRepo(repo.ID); err == nil {
				t.Fatal("control database left open")
			}
		})
	}
}
