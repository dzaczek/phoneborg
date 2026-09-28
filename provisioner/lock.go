package provisioner

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Exclusivity: only one "pcprov watch" or "pcprov provision" should drive a
// given adb server at a time (docs/DECISIONS.md ADR-019). Two instances
// provisioning the same phone concurrently could race on the same port
// forwards and agent restart, and Watch's own backoff/report state is only
// meaningful for a single process. Lock (see lock_unix.go / lock_other.go)
// takes an exclusive, non-blocking flock on a file keyed by the adb server
// port, so a second instance fails fast with a clear error instead of
// silently fighting the first one.

// defaultADBServerPort is adb's own default when $ANDROID_ADB_SERVER_PORT is
// unset (matches adb's -P/ANDROID_ADB_SERVER_PORT convention).
const defaultADBServerPort = 5037

// ADBServerPort returns the adb server port pcprov's adb calls use:
// $ANDROID_ADB_SERVER_PORT (adb's own override) if set, else adb's default.
func ADBServerPort() int {
	if v := os.Getenv("ANDROID_ADB_SERVER_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			return p
		}
	}
	return defaultADBServerPort
}

// LockPath is the exclusivity lock file for the adb server at port. It is
// always under os.TempDir(), never a per-session directory such as
// $XDG_RUNTIME_DIR: the adb server port is host-wide, and a systemd service
// (no $XDG_RUNTIME_DIR) and an interactive shell must contend for one file.
func LockPath(port int) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("pcprov-adb-%d.lock", port))
}
