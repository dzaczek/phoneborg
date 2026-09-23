// Package provisioner installs and starts the node agent on Android devices
// over ADB. Every adb call targets an explicit serial so unrelated attached
// devices are never touched.
package provisioner

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type Device struct {
	Serial string
	State  string // device, unauthorized, offline, ...
	Model  string
}

// ParseDevices parses `adb devices -l`.
func ParseDevices(out string) []Device {
	var devs []Device
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || strings.HasPrefix(line, "List of devices") || strings.HasPrefix(line, "*") {
			continue
		}
		d := Device{Serial: f[0], State: f[1]}
		for _, kv := range f[2:] {
			if v, ok := strings.CutPrefix(kv, "model:"); ok {
				d.Model = v
			}
		}
		devs = append(devs, d)
	}
	return devs
}

type ADB struct {
	Bin     string
	Timeout time.Duration
}

func (a ADB) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, a.Timeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, a.Bin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("adb %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()+stdout.String()))
	}
	return stdout.String(), nil
}

func (a ADB) Devices(ctx context.Context) ([]Device, error) {
	out, err := a.run(ctx, "devices", "-l")
	if err != nil {
		return nil, err
	}
	return ParseDevices(out), nil
}

func (a ADB) Connect(ctx context.Context, hostPort string) error {
	out, err := a.run(ctx, "connect", hostPort)
	if err == nil && (strings.Contains(out, "failed") || strings.Contains(out, "cannot")) {
		err = fmt.Errorf("adb connect %s: %s", hostPort, strings.TrimSpace(out))
	}
	return err
}

func (a ADB) Shell(ctx context.Context, serial, cmd string) (string, error) {
	return a.run(ctx, "-s", serial, "shell", cmd)
}

func (a ADB) Push(ctx context.Context, serial, local, remote string) error {
	_, err := a.run(ctx, "-s", serial, "push", local, remote)
	return err
}

func (a ADB) Reverse(ctx context.Context, serial string, port int) error {
	spec := fmt.Sprintf("tcp:%d", port)
	_, err := a.run(ctx, "-s", serial, "reverse", spec, spec)
	return err
}

// ParseForwardList finds the host port of an existing `adb forward` from
// serial to device tcp:remotePort in `adb forward --list` output.
func ParseForwardList(out, serial string, remotePort int) (int, bool) {
	want := fmt.Sprintf("tcp:%d", remotePort)
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[0] == serial && f[2] == want {
			if p, err := strconv.Atoi(strings.TrimPrefix(f[1], "tcp:")); err == nil {
				return p, true
			}
		}
	}
	return 0, false
}

// Forward makes device tcp:remotePort reachable on host 127.0.0.1, reusing an
// existing forward so the host port stays stable across re-provisioning.
func (a ADB) Forward(ctx context.Context, serial string, remotePort int) (int, error) {
	list, err := a.run(ctx, "forward", "--list")
	if err != nil {
		return 0, err
	}
	if p, ok := ParseForwardList(list, serial, remotePort); ok {
		return p, nil
	}
	out, err := a.run(ctx, "-s", serial, "forward", "tcp:0", fmt.Sprintf("tcp:%d", remotePort))
	if err != nil {
		return 0, err
	}
	p, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("adb forward: unexpected output %q", out)
	}
	return p, nil
}
