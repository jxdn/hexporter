package collectors

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func procPath(path string) string {
	base := os.Getenv("HOST_PROC")
	if base == "" {
		base = "/proc"
	}
	return filepath.Join(base, path)
}

func sysPath(path string) string {
	base := os.Getenv("HOST_SYS")
	if base == "" {
		base = "/sys"
	}
	return filepath.Join(base, path)
}

// runCmd executes a binary with a 10-second timeout and returns combined output.
// Returns an error if the binary is not found or the command fails.
func runCmd(name string, args ...string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).Output()
	return string(out), err
}

// binaryExists returns true if the named binary is in PATH.
func binaryExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
