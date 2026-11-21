package provisioner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// A /24 is the common home-LAN case; it once expanded the wrong bits and
// spun forever, so pin its size and both ends.
func TestParseScanTargetsCIDR24(t *testing.T) {
	got, err := ParseScanTargets([]string{"192.168.1.0/24"}, 5555)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 254 || got[0] != "192.168.1.1:5555" || got[253] != "192.168.1.254:5555" {
		t.Fatalf("got %d targets [%s .. %s], want 254 [192.168.1.1:5555 .. 192.168.1.254:5555]", len(got), got[0], got[len(got)-1])
	}
}

func TestParseScanTargets(t *testing.T) {
	tests := []struct {
		name    string
		specs   []string
		want    []string
		wantErr string
	}{
		{"single", []string{"192.168.1.50"}, []string{"192.168.1.50:5555"}, ""},
		{"single with port", []string{"192.168.1.50:5556"}, []string{"192.168.1.50:5556"}, ""},
		{"cidr skips network and broadcast", []string{"192.168.1.0/30"}, []string{"192.168.1.1:5555", "192.168.1.2:5555"}, ""},
		{"cidr /31 keeps both", []string{"192.168.1.4/31"}, []string{"192.168.1.4:5555", "192.168.1.5:5555"}, ""},
		{"cidr /32", []string{"192.168.1.9/32"}, []string{"192.168.1.9:5555"}, ""},
		{"range at the top of the address space", []string{"255.255.255.254-255"}, []string{"255.255.255.254:5555", "255.255.255.255:5555"}, ""},
		{"cidr with host bits is masked", []string{"192.168.1.77/30"}, []string{"192.168.1.77:5555", "192.168.1.78:5555"}, ""},
		{"full range", []string{"192.168.1.10-192.168.1.12"}, []string{"192.168.1.10:5555", "192.168.1.11:5555", "192.168.1.12:5555"}, ""},
		{"short range", []string{"192.168.1.10-12"}, []string{"192.168.1.10:5555", "192.168.1.11:5555", "192.168.1.12:5555"}, ""},
		{"range of one", []string{"192.168.1.10-10"}, []string{"192.168.1.10:5555"}, ""},
		{"dedupe keeps first order", []string{"10.0.0.2", "10.0.0.1-3", "10.0.0.2"}, []string{"10.0.0.2:5555", "10.0.0.1:5555", "10.0.0.3:5555"}, ""},
		{"same host different port is distinct", []string{"10.0.0.1", "10.0.0.1:5556"}, []string{"10.0.0.1:5555", "10.0.0.1:5556"}, ""},
		{"cap exactly 4096 ok", []string{"10.0.0.0-10.0.15.255"}, nil, ""},
		{"cap exceeded", []string{"10.0.0.0-10.0.16.0"}, nil, "4096"},
		{"huge cidr", []string{"10.0.0.0/8"}, nil, "4096"},
		{"cap exceeded across specs", []string{"10.0.0.0-10.0.15.255", "10.1.0.1"}, nil, "4096"},
		{"reversed range", []string{"192.168.1.40-10"}, nil, "reversed"},
		{"ipv6", []string{"::1"}, nil, "IPv4"},
		{"ipv6 with port", []string{"[::1]:5555"}, nil, "IPv4"},
		{"ipv6 cidr", []string{"fe80::/64"}, nil, "IPv4"},
		{"garbage", []string{"tvbox"}, nil, "IPv4"},
		{"empty", []string{""}, nil, "empty"},
		{"bad octet", []string{"192.168.1.256"}, nil, "IPv4"},
		{"bad short end", []string{"192.168.1.1-300"}, nil, "octet"},
		{"bad cidr", []string{"192.168.1.0/33"}, nil, "CIDR"},
		{"port zero", []string{"192.168.1.1:0"}, nil, "IPv4"},
		{"port on range not allowed", []string{"192.168.1.1-5:5556"}, nil, "range"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseScanTargets(tc.specs, 5555)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == nil {
				if len(got) != 4096 {
					t.Fatalf("got %d targets, want 4096", len(got))
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseScanTargetsBadDefaultPort(t *testing.T) {
	if _, err := ParseScanTargets([]string{"10.0.0.1"}, 0); err == nil {
		t.Fatal("want error for port 0")
	}
}

type fakeADB struct {
	mu          sync.Mutex
	devs        []Device
	devErr      error
	failOn      map[string]bool
	connects    []string
	disconnects []string
}

func (f *fakeADB) Devices(context.Context) ([]Device, error) { return f.devs, f.devErr }
func (f *fakeADB) Disconnect(_ context.Context, hp string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnects = append(f.disconnects, hp)
	return nil
}
func (f *fakeADB) Connect(_ context.Context, hp string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connects = append(f.connects, hp)
	if f.failOn[hp] {
		return errors.New("boom")
	}
	return nil
}

func fakeDial(open ...string) func(context.Context, string, string) (net.Conn, error) {
	set := map[string]bool{}
	for _, o := range open {
		set[o] = true
	}
	return func(_ context.Context, _, addr string) (net.Conn, error) {
		if !set[addr] {
			return nil, errors.New("refused")
		}
		c, s := net.Pipe()
		_ = s.Close()
		return c, nil
	}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestScanOnce(t *testing.T) {
	targets := []string{"10.0.0.1:5555", "10.0.0.2:5555", "10.0.0.3:5555", "10.0.0.4:5555", "10.0.0.5:5555", "10.0.0.6:5555"}
	adb := &fakeADB{
		devs: []Device{
			{Serial: "10.0.0.1:5555", State: "device"},       // already connected
			{Serial: "10.0.0.2:5555", State: "unauthorized"}, // waiting for TV prompt
			{Serial: "10.0.0.3:5555", State: "offline"},      // reconnect
			{Serial: "USB123", State: "device"},
		},
		failOn: map[string]bool{"10.0.0.4:5555": true},
	}
	s := &Scanner{
		ADB: adb, Targets: targets, Log: quietLog(), Concurrency: 2,
		// 10.0.0.6 is closed.
		Dial: fakeDial("10.0.0.1:5555", "10.0.0.2:5555", "10.0.0.3:5555", "10.0.0.4:5555", "10.0.0.5:5555"),
	}
	got := s.ScanOnce(context.Background())
	// .4 fails to connect but must not stop .5.
	if want := []string{"10.0.0.3:5555", "10.0.0.5:5555"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("connected = %v, want %v", got, want)
	}
	if want := []string{"10.0.0.3:5555", "10.0.0.4:5555", "10.0.0.5:5555"}; !reflect.DeepEqual(adb.connects, want) {
		t.Fatalf("Connect calls = %v, want %v", adb.connects, want)
	}
	// Only the offline entry is dropped first: adb ignores a connect on it.
	if want := []string{"10.0.0.3:5555"}; !reflect.DeepEqual(adb.disconnects, want) {
		t.Fatalf("Disconnect calls = %v, want %v", adb.disconnects, want)
	}
}

func TestScanOnceDevicesError(t *testing.T) {
	adb := &fakeADB{devErr: errors.New("adb down")}
	s := &Scanner{ADB: adb, Targets: []string{"10.0.0.1:5555"}, Log: quietLog(), Dial: fakeDial("10.0.0.1:5555")}
	if got := s.ScanOnce(context.Background()); got != nil || len(adb.connects) != 0 {
		t.Fatalf("connected %v / %v despite devices error", got, adb.connects)
	}
}

func TestScannerRunStopsOnCancel(t *testing.T) {
	adb := &fakeADB{}
	s := &Scanner{ADB: adb, Targets: []string{"10.0.0.1:5555"}, Log: quietLog(), Dial: fakeDial("10.0.0.1:5555")}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx, 5*time.Millisecond); close(done) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	adb.mu.Lock()
	defer adb.mu.Unlock()
	if len(adb.connects) == 0 {
		t.Fatal("Run never scanned")
	}
}

func TestScannerSatisfiesADB(t *testing.T) {
	var _ scanADB = ADB{}
}

func TestWatchInclude(t *testing.T) {
	scan := IsScanTarget([]string{"10.0.0.5:5555"})
	tests := []struct {
		name    string
		allowed map[string]bool
		scan    func(string) bool
		serial  string
		want    bool
	}{
		{"no filters includes USB", nil, nil, "USB123", true},
		{"scan only still includes USB", map[string]bool{}, scan, "USB123", true},
		{"scan only includes scanned", map[string]bool{}, scan, "10.0.0.5:5555", true},
		{"allow-list excludes others", map[string]bool{"A": true}, nil, "USB123", false},
		{"allow-list includes listed", map[string]bool{"A": true}, nil, "A", true},
		{"allow-list plus scan includes scanned", map[string]bool{"A": true}, scan, "10.0.0.5:5555", true},
		{"allow-list plus scan excludes unlisted USB", map[string]bool{"A": true}, scan, "USB123", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := WatchInclude(tc.allowed, tc.scan)(tc.serial); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
