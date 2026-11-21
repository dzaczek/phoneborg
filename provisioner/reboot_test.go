package provisioner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fakeSerial = "192.168.44.213:5555"

// newFakeADB writes a POSIX sh script that stands in for adb. It lists one TV box
// as "device", answers every call Provision and Heal make, and keeps the
// agent's state ("running" or "gone") in a file the test can flip to simulate
// a reboot. Every invocation is appended to the returned log file.
func newFakeADB(t *testing.T) (adb ADB, state, logFile string) {
	t.Helper()
	dir := t.TempDir()
	state, logFile = filepath.Join(dir, "state"), filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
echo "$*" >> "` + logFile + `"
if [ "$1" = devices ]; then
  echo "List of devices attached"
  echo "` + fakeSerial + ` device model:Box"
  exit 0
fi
if [ "$1" = forward ] && [ "$2" = --list ]; then exit 0; fi
[ "$1" = -s ] && shift 2
case "$1" in
  reverse) [ "$2" = --list ] && echo "host-13 tcp:18080 tcp:18080" ;;
  shell)
    case "$2" in
      *getprop*) echo arm64-v8a ;;
      *"cat /proc/"*)
        case "$(cat "` + state + `")" in
          running) echo "./node-agent --controller http://127.0.0.1:18080" ;;
          adbfail) echo "error: closed" >&2; exit 1 ;; # the adb transport itself failed
        esac ;;
      *"mv -f node-agent.new"*) echo running > "` + state + `"; echo 4242 ;;
    esac ;;
esac
exit 0
`
	bin := filepath.Join(dir, "adb")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, []byte("gone\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return ADB{Bin: bin, Timeout: 5 * time.Second}, state, logFile
}

func fakeProvisioner(t *testing.T, adb ADB) *Provisioner {
	agent := filepath.Join(t.TempDir(), "node-agent")
	if err := os.WriteFile(agent, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Provisioner{
		ADB:  adb,
		Opts: Options{AgentBinary: agent, ControllerPort: 18080, ServePort: 18090},
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func countCalls(t *testing.T, logFile, substr string) int {
	b, _ := os.ReadFile(logFile)
	return strings.Count(string(b), substr)
}

func TestHealAgentNotRunning(t *testing.T) {
	adb, state, _ := newFakeADB(t)
	p := fakeProvisioner(t, adb)
	ctx := context.Background()

	if _, err := p.Heal(ctx, fakeSerial); !errors.Is(err, ErrAgentNotRunning) {
		t.Fatalf("gone agent: err = %v, want ErrAgentNotRunning", err)
	}
	if err := os.WriteFile(state, []byte("running\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fixed, err := p.Heal(ctx, fakeSerial); err != nil || len(fixed) != 0 {
		t.Fatalf("running agent without advertise port: fixed=%v err=%v, want nil", fixed, err)
	}
	// A failing adb call is an error, not a dead agent: it must not trigger
	// a re-provision (which would restart the model on a healthy phone).
	if err := os.WriteFile(state, []byte("adbfail\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Heal(ctx, fakeSerial); err == nil || errors.Is(err, ErrAgentNotRunning) {
		t.Fatalf("adb failure: err = %v, want a plain error", err)
	}
}

// A rebooted network-adb device never leaves the adb list, so Watch must
// notice the dead agent itself and provision again.
func TestWatchReprovisionsAfterReboot(t *testing.T) {
	old := healInterval
	healInterval = 10 * time.Millisecond
	t.Cleanup(func() { healInterval = old })

	adb, state, logFile := newFakeADB(t)
	p := fakeProvisioner(t, adb)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.Watch(ctx, 20*time.Millisecond, WatchOptions{})
	}()
	t.Cleanup(func() { cancel(); <-done })

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				b, _ := os.ReadFile(logFile)
				t.Fatalf("timed out waiting for %s; adb calls:\n%s", what, b)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	pushes := func() int { return countCalls(t, logFile, " push ") }

	waitFor("first provision", func() bool { return pushes() == 1 })
	// Let a few healthy ticks pass: a running agent must not be re-provisioned.
	time.Sleep(150 * time.Millisecond)
	if n := pushes(); n != 1 {
		t.Fatalf("healthy device re-provisioned: %d pushes", n)
	}

	if err := os.WriteFile(state, []byte("gone\n"), 0o644); err != nil { // reboot
		t.Fatal(err)
	}
	waitFor("re-provision after reboot", func() bool { return pushes() >= 2 })
}
