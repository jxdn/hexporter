package collectors

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// procPath returns a path under the host's /proc (HOST_PROC, default /proc).
func procPath(path string) string {
	base := os.Getenv("HOST_PROC")
	if base == "" {
		base = "/proc"
	}
	return filepath.Join(base, path)
}

// sysPath returns a path under the host's /sys (HOST_SYS, default /sys).
func sysPath(path string) string {
	base := os.Getenv("HOST_SYS")
	if base == "" {
		base = "/sys"
	}
	return filepath.Join(base, path)
}

// hostPath maps an absolute host path (e.g. a mountpoint) to where it is
// visible to this process: HOST_ROOT + path. HOST_ROOT is empty on bare metal
// and /host/root inside the container.
func hostPath(path string) string {
	root := os.Getenv("HOST_ROOT")
	if root == "" || root == "/" {
		return path
	}
	return filepath.Join(root, path)
}

// runCmd executes a binary with a 10-second timeout and returns stdout.
func runCmd(name string, args ...string) (string, error) {
	return runCmdTimeout(10*time.Second, name, args...)
}

// extraBinDirs are searched after PATH: vendor install folders that are often
// not on a service's PATH. EXTRA_PATH (colon-separated) is searched first.
var extraBinDirs = []string{
	"/cm/local/apps/cuda-dcgm/current/bin", // Bright Cluster Manager DCGM (dcgmi)
	"/usr/local/dcgm/bin",
	"/opt/pbs/bin",
	"/usr/sbin",
	"/sbin",
}

// lookBin finds a binary in PATH, then EXTRA_PATH, then extraBinDirs.
func lookBin(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err == nil {
		return path, nil
	}
	dirs := append(filepath.SplitList(os.Getenv("EXTRA_PATH")), extraBinDirs...)
	for _, d := range dirs {
		if d == "" {
			continue
		}
		p := filepath.Join(d, name)
		if st, e := os.Stat(p); e == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", err
}

// runCmdTimeout executes a binary with the given timeout and returns stdout.
// Returns an error if the binary is not found or the command fails.
func runCmdTimeout(timeout time.Duration, name string, args ...string) (string, error) {
	path, err := lookBin(name)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).Output()
	return string(out), err
}

// binaryExists returns true if the named binary is in PATH.
func binaryExists(name string) bool {
	_, err := lookBin(name)
	return err == nil
}

// envBool reads a boolean env var ("false"/"0"/"no" = false), default def.
func envBool(name string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	switch v {
	case "":
		return def
	case "false", "0", "no", "off":
		return false
	default:
		return true
	}
}

// envDuration reads seconds ("5") or a Go duration ("5s"), default def.
func envDuration(name string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		return time.Duration(n * float64(time.Second))
	}
	return def
}

// envList reads a comma-separated env var into a trimmed, non-empty list.
func envList(name string) []string {
	var out []string
	for _, s := range strings.Split(os.Getenv(name), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
