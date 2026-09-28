package provisioner

import (
	"errors"
	"testing"
	"time"

	"github.com/dzaczek/phoneborg/proto"
)

func TestNextBackoff(t *testing.T) {
	cases := []struct {
		prev, want time.Duration
	}{
		{0, BackoffMin},
		{BackoffMin, 2 * BackoffMin},
		{2 * BackoffMin, 4 * BackoffMin},
		{BackoffMax / 2, BackoffMax},
		{BackoffMax, BackoffMax},
		{BackoffMax * 10, BackoffMax}, // never overshoots even if called with a stale, too-large value
	}
	for _, c := range cases {
		if got := NextBackoff(c.prev); got != c.want {
			t.Errorf("NextBackoff(%s) = %s, want %s", c.prev, got, c.want)
		}
	}
}

func TestIsPlaceholderSerial(t *testing.T) {
	for _, s := range []string{"0123456789ABCDEF", "0123456789abcdef", "unknown", "UNKNOWN"} {
		if !IsPlaceholderSerial(s) {
			t.Errorf("%q should be a placeholder", s)
		}
	}
	for _, s := range []string{"", "R58N123ABCD", "0123456789ABCDE"} {
		if IsPlaceholderSerial(s) {
			t.Errorf("%q should not be a placeholder", s)
		}
	}
}

func TestDuplicateSerials(t *testing.T) {
	devs := []Device{{Serial: "A"}, {Serial: "B"}, {Serial: "A"}}
	dup := DuplicateSerials(devs)
	if !dup["A"] || dup["B"] {
		t.Fatalf("dup = %+v", dup)
	}
	if len(DuplicateSerials([]Device{{Serial: "A"}})) != 0 {
		t.Fatal("single device flagged as duplicate")
	}
}

func TestPlanDeviceDuplicateAndPlaceholderNeverProvision(t *testing.T) {
	rec := &deviceRecord{}
	now := time.Now()
	for _, forced := range []bool{false, true} {
		if p := planDevice(rec, true, false, forced, true, now); p.Attempt || p.Status != proto.DeviceFailed {
			t.Errorf("duplicate forced=%v: %+v", forced, p)
		}
		if p := planDevice(rec, false, true, forced, true, now); p.Attempt || p.Status != proto.DeviceFailed {
			t.Errorf("placeholder forced=%v: %+v", forced, p)
		}
	}
}

func TestPlanDeviceAutoProvisionOff(t *testing.T) {
	rec := &deviceRecord{}
	now := time.Now()
	// auto_provision off, no operator request: stays "new", never attempts.
	if p := planDevice(rec, false, false, false, false, now); p.Attempt || p.Status != proto.DeviceNew {
		t.Fatalf("auto off, new device: %+v", p)
	}
	// An explicit "provision now" bypasses auto_provision=false.
	if p := planDevice(rec, false, false, true, false, now); !p.Attempt {
		t.Fatalf("forced should attempt even with auto_provision off: %+v", p)
	}
}

func TestPlanDeviceBackoffGating(t *testing.T) {
	now := time.Now()
	rec := &deviceRecord{nextAttempt: now.Add(time.Minute)}
	rec.Status = proto.DeviceFailed
	// Auto-provisioning respects the backoff: no attempt yet, keeps showing failed.
	if p := planDevice(rec, false, false, false, true, now); p.Attempt || p.Status != proto.DeviceFailed {
		t.Fatalf("within backoff: %+v", p)
	}
	// An explicit retry bypasses the backoff.
	if p := planDevice(rec, false, false, true, true, now); !p.Attempt {
		t.Fatalf("retry should bypass backoff: %+v", p)
	}
	// Once nextAttempt has passed, auto-provisioning retries on its own.
	if p := planDevice(rec, false, false, false, true, now.Add(2*time.Minute)); !p.Attempt {
		t.Fatalf("after backoff elapsed: %+v", p)
	}
}

func TestPlanDeviceAlreadyProvisioned(t *testing.T) {
	rec := &deviceRecord{provisioned: true}
	now := time.Now()
	if p := planDevice(rec, false, false, false, true, now); p.Attempt || p.Status != proto.DeviceProvisioned {
		t.Fatalf("provisioned, no force: %+v", p)
	}
	// A forced re-provision (e.g. push an updated agent) is still honoured.
	if p := planDevice(rec, false, false, true, true, now); !p.Attempt {
		t.Fatalf("forced re-provision of an already-provisioned device: %+v", p)
	}
}

func TestHintForADBState(t *testing.T) {
	if hintForADBState("unauthorized") == "" || hintForADBState("offline") == "" {
		t.Fatal("known adb states should have a hint")
	}
	if hintForADBState("something-else") != "" {
		t.Fatal("unknown adb state should have no hint")
	}
}

func TestHintForProvisionError(t *testing.T) {
	if hintForProvisionError(errors.New("device x: unsupported ABI list \"armeabi-v7a\"")) == "" {
		t.Fatal("unsupported ABI should have a hint")
	}
	if hintForProvisionError(errors.New("something unrelated broke")) != "" {
		t.Fatal("unrecognised error should have no hint")
	}
}
