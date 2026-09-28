package provisioner

import (
	"strings"
	"time"

	"github.com/dzaczek/phoneborg/proto"
)

// Device reporting to the controller and conflict-free provisioning
// (docs/DECISIONS.md ADR-019): Watch's per-serial bookkeeping, backoff and
// the decisions that follow from it. Kept as pure functions/types wherever
// possible (no adb calls), the same way the rest of this package tests adb
// output parsing (ParseDevices, ParseForwardList, ...) without a real adb
// server: only Watch itself drives real Provision calls.

// Backoff bounds for a failed provisioning attempt: the first retry waits
// BackoffMin, doubling on each further failure up to BackoffMax. Replugging
// the device (gone, then back) or an explicit operator retry resets it.
const (
	BackoffMin = 30 * time.Second
	BackoffMax = 10 * time.Minute
)

// NextBackoff returns the backoff to wait after another failure, given the
// previous one (0 = no prior failure).
func NextBackoff(prev time.Duration) time.Duration {
	if prev < BackoffMin {
		return BackoffMin
	}
	if prev >= BackoffMax/2 {
		return BackoffMax
	}
	return prev * 2
}

// placeholderSerials are known junk serials some devices or bad USB
// descriptors report; never mistaken for a real, provisionable phone.
var placeholderSerials = map[string]bool{
	"0123456789ABCDEF": true,
	"UNKNOWN":          true,
}

// IsPlaceholderSerial reports whether serial is a known placeholder/junk
// value rather than a real device identity.
func IsPlaceholderSerial(serial string) bool {
	return placeholderSerials[strings.ToUpper(strings.TrimSpace(serial))]
}

// DuplicateSerials returns the serials that appear more than once in devs:
// two devices reporting the same serial (a bad USB hub, a cloned identity).
// "adb -s <serial>" would then be ambiguous, so none of them are safe to
// provision.
func DuplicateSerials(devs []Device) map[string]bool {
	count := map[string]int{}
	for _, d := range devs {
		count[d.Serial]++
	}
	dup := map[string]bool{}
	for s, n := range count {
		if n > 1 {
			dup[s] = true
		}
	}
	return dup
}

// deviceRecord is Watch's per-serial bookkeeping, on top of the
// proto.DeviceStatus reported to the controller.
type deviceRecord struct {
	proto.DeviceStatus
	provisioned bool          // provisioned successfully this run; only adb links are healed from here on
	backoff     time.Duration // current backoff after a failed attempt; 0 = none yet
	nextAttempt time.Time     // auto-provisioning waits until this time after a failure
}

// devicePlan is what Watch should do this tick for one ready ("device"
// adb state) serial.
type devicePlan struct {
	Attempt bool   // run Provision now
	Status  string // the DeviceStatus to report
}

// planDevice decides what to do with a ready device, given its bookkeeping
// and the current settings. It is pure (no adb, no clock reads beyond the
// passed-in now), so auto_provision=false behaviour and backoff gating are
// unit-tested directly.
//
// forced (an operator "Provision now"/"Retry") always attempts, bypassing
// auto_provision, backoff and even an already-successful provision (e.g. to
// push an updated agent) — except for a duplicate or placeholder serial,
// which is never provisioned regardless.
func planDevice(rec *deviceRecord, duplicate, placeholder, forced, autoProvision bool, now time.Time) devicePlan {
	switch {
	case duplicate, placeholder:
		return devicePlan{Status: proto.DeviceFailed}
	case forced:
		return devicePlan{Attempt: true, Status: proto.DeviceProvisioning}
	case rec.provisioned:
		return devicePlan{Status: proto.DeviceProvisioned}
	case autoProvision && !now.Before(rec.nextAttempt):
		return devicePlan{Attempt: true, Status: proto.DeviceProvisioning}
	case rec.Status == proto.DeviceFailed:
		return devicePlan{Status: proto.DeviceFailed} // keep showing the last failure until the next attempt
	default:
		return devicePlan{Status: proto.DeviceNew}
	}
}

// hintForADBState gives an operator-facing suggestion for a non-"device" adb
// state, matching docs/REAL_PHONES.md's troubleshooting table.
func hintForADBState(state string) string {
	switch state {
	case "unauthorized":
		return "unlock the phone and accept the USB debugging prompt (tick \"always allow\")"
	case "offline":
		return "run 'adb kill-server && adb start-server', or replug the cable"
	default:
		return ""
	}
}

// hintForProvisionError gives an operator-facing suggestion for a
// provisioning failure, by matching known error text (best-effort; an
// unrecognised error gets no hint beyond the raw message).
func hintForProvisionError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unsupported ABI"):
		return "not an arm64 phone: unsupported"
	case strings.Contains(msg, "no llama.cpp build matches"):
		return "run 'make llama-all' to build a variant for this phone's CPU"
	case strings.Contains(msg, "agent did not start"):
		return "check agent.log on the device (pcprov status -serial <serial>)"
	default:
		return ""
	}
}
