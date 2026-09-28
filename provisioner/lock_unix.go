//go:build linux || darwin

package provisioner

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Lock takes an exclusive, non-blocking flock on path, recording this
// process's PID in it so a second instance can name the holder. The returned
// release function drops the lock; it does not remove the file (removing it
// would race a third instance opening it between unlock and remove, which
// could then hold a lock on an unlinked inode nobody else sees).
func Lock(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := strings.TrimSpace(readPID(f))
		f.Close()
		if holder == "" {
			holder = "unknown"
		}
		return nil, fmt.Errorf("another pcprov (pid %s) is already watching/provisioning this adb server (lock file: %s)", holder, path)
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func readPID(f *os.File) string {
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	return string(buf[:n])
}
