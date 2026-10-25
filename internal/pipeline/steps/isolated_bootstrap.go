package steps

import (
	"os"
	"path/filepath"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/testisolation"
)

// bootstrapIsolatedWorker owns the fixed runtime shared by both namespace
// workers. Agent configuration and step execution remain with each caller.
func bootstrapIsolatedWorker() (*db.DB, func(), error) {
	return bootstrapIsolatedWorkerAt("/nm/state.db", "/tmp", testisolation.ServePrivateServices)
}

// Paths and service startup are explicit here so offline tests can exercise
// the same lifecycle without entering a namespace or starting native services.
func bootstrapIsolatedWorkerAt(statePath, runtimeDir string, serve func() (func(), error)) (*db.DB, func(), error) {
	state, err := os.ReadFile(statePath)
	if err != nil {
		return nil, nil, err
	}
	controlPath := filepath.Join(runtimeDir, "control.db")
	if err := os.WriteFile(controlPath, state, 0600); err != nil {
		return nil, nil, err
	}
	database, err := db.Open(controlPath)
	if err != nil {
		return nil, nil, err
	}
	closeServices, err := serve()
	if err != nil {
		database.Close()
		return nil, nil, err
	}
	cleanup := func() { closeServices(); database.Close() }
	// Invocation-local dummy, not a provider credential: the host bridge
	// strips it and supplies the configured host-side key itself.
	if err := os.WriteFile(filepath.Join(runtimeDir, "model-client-key"), []byte("isolated-client"), 0600); err != nil {
		cleanup()
		return nil, nil, err
	}
	return database, cleanup, nil
}
