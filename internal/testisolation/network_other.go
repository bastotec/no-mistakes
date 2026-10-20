//go:build !linux

package testisolation

import (
	"fmt"
	"net"
	"os"
)

func RestrictWorkerNetwork() error { return fmt.Errorf("test-isolation-platform: Linux is required") }
func socketFilter() ([]byte, error) {
	return nil, fmt.Errorf("test-isolation-platform: Linux is required")
}
func modelChannel() (net.Conn, *os.File, error) {
	return nil, nil, fmt.Errorf("test-isolation-platform: Linux is required")
}
