package provisioner

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// adb drops `adb reverse` and `adb forward` rules when a phone re-enumerates
// on USB (a loose cable, a hot phone briefly disconnecting). The agent then
// cannot reach the controller, or the gateway cannot reach llama-server,
// while adb still lists the device as connected. Heal restores both.

// ParseAdvertisePort extracts the node agent's --advertise-port value from its
// command line (arguments separated by spaces or NULs).
func ParseAdvertisePort(cmdline string) (int, bool) {
	f := strings.Fields(strings.ReplaceAll(cmdline, "\x00", " "))
	for i, a := range f {
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if name != "advertise-port" {
			continue
		}
		if !hasVal {
			if i+1 >= len(f) {
				return 0, false
			}
			val = f[i+1]
		}
		p, err := strconv.Atoi(val)
		return p, err == nil && p > 0
	}
	return 0, false
}

// ReverseListed reports whether `adb reverse --list` output maps device
// tcp:port to host tcp:port.
func ReverseListed(out string, port int) bool {
	want := fmt.Sprintf("tcp:%d tcp:%d", port, port)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), want) {
			return true
		}
	}
	return false
}

// Heal restores a provisioned device's missing adb reverse (phone to
// controller) and adb forward (controller to llama-server). The forward is
// recreated on the host port the running agent advertises, so the controller's
// view of the node stays valid. It returns what was restored.
func (p *Provisioner) Heal(ctx context.Context, serial string) ([]string, error) {
	var fixed []string
	rev, err := p.ADB.run(ctx, "-s", serial, "reverse", "--list")
	if err != nil {
		return nil, err
	}
	if !ReverseListed(rev, p.Opts.ControllerPort) {
		if err := p.ADB.Reverse(ctx, serial, p.Opts.ControllerPort); err != nil {
			return fixed, err
		}
		fixed = append(fixed, fmt.Sprintf("reverse tcp:%d", p.Opts.ControllerPort))
	}

	cmdline, _ := p.ADB.Shell(ctx, serial, "cat /proc/$(cat "+RemoteDir+"/agent.pid 2>/dev/null)/cmdline 2>/dev/null")
	port, ok := ParseAdvertisePort(cmdline)
	if !ok {
		return fixed, nil // agent not running or not serving: nothing to forward
	}
	list, err := p.ADB.run(ctx, "forward", "--list")
	if err != nil {
		return fixed, err
	}
	if cur, ok := ParseForwardList(list, serial, p.Opts.ServePort); !ok || cur != port {
		if _, err := p.ADB.run(ctx, "-s", serial, "forward", fmt.Sprintf("tcp:%d", port), fmt.Sprintf("tcp:%d", p.Opts.ServePort)); err != nil {
			return fixed, err
		}
		fixed = append(fixed, fmt.Sprintf("forward tcp:%d->%d", port, p.Opts.ServePort))
	}
	if len(fixed) > 0 {
		p.Log.Warn("adb links restored", "serial", serial, "restored", fixed)
	}
	return fixed, nil
}
