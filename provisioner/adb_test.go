package provisioner

import "testing"

func TestParseDevices(t *testing.T) {
	out := `* daemon started successfully
List of devices attached
127.0.0.1:5555         device product:redroid_arm64_only model:redroid12_arm64_only device:redroid_arm64_only transport_id:3
R58M123ABC             unauthorized usb:1-1 transport_id:4
emulator-5554          offline transport_id:1

`
	d := ParseDevices(out)
	if len(d) != 3 {
		t.Fatalf("got %d devices: %+v", len(d), d)
	}
	if d[0].Serial != "127.0.0.1:5555" || d[0].State != "device" || d[0].Model != "redroid12_arm64_only" {
		t.Fatalf("device 0: %+v", d[0])
	}
	if d[1].State != "unauthorized" || d[2].State != "offline" {
		t.Fatalf("states: %+v", d)
	}
}

func TestSupportedABI(t *testing.T) {
	if !SupportedABI("arm64-v8a,armeabi-v7a,armeabi\n") {
		t.Fatal("arm64 phone rejected")
	}
	if !SupportedABI("arm64-v8a\n") {
		t.Fatal("64-only phone rejected")
	}
	if SupportedABI("armeabi-v7a,armeabi") || SupportedABI("x86_64,x86") {
		t.Fatal("non-arm64 accepted")
	}
}

func TestParseForwardList(t *testing.T) {
	out := "127.0.0.1:5555 tcp:40123 tcp:18090\n127.0.0.1:5556 tcp:40124 tcp:18090\n127.0.0.1:5555 tcp:5000 tcp:9999\n"
	if p, ok := ParseForwardList(out, "127.0.0.1:5556", 18090); !ok || p != 40124 {
		t.Fatalf("got %d %v", p, ok)
	}
	if _, ok := ParseForwardList(out, "other", 18090); ok {
		t.Fatal("matched wrong serial")
	}
}
