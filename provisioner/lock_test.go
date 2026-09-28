//go:build linux || darwin

package provisioner

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLockExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pcprov.lock")
	release, err := Lock(path)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	defer release()

	_, err = Lock(path)
	if err == nil {
		t.Fatal("second lock on the same file should fail")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
		t.Fatalf("error does not name the holder PID: %v", err)
	}

	release()
	release2, err := Lock(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	release2()
}

func TestLockPathIgnoresSessionDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	a := LockPath(5037)
	t.Setenv("XDG_RUNTIME_DIR", "")
	if b := LockPath(5037); a != b {
		t.Fatalf("lock path depends on XDG_RUNTIME_DIR: %q vs %q", a, b)
	}
}

func TestLockPathKeyedByPort(t *testing.T) {
	if LockPath(5037) == LockPath(5038) {
		t.Fatal("lock paths for different adb server ports collide")
	}
}

func TestADBServerPortDefaultAndOverride(t *testing.T) {
	t.Setenv("ANDROID_ADB_SERVER_PORT", "")
	if p := ADBServerPort(); p != defaultADBServerPort {
		t.Fatalf("default = %d, want %d", p, defaultADBServerPort)
	}
	t.Setenv("ANDROID_ADB_SERVER_PORT", "6000")
	if p := ADBServerPort(); p != 6000 {
		t.Fatalf("override = %d, want 6000", p)
	}
}
