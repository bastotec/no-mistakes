//go:build !linux

package testisolation

import (
	"fmt"
	"os"
)

func openRepositoryDir(*os.File, string) (*os.File, error) {
	return nil, fmt.Errorf("test-isolation-repository: safe directory inspection requires Linux")
}
