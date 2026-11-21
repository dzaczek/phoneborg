package provisioner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxScanHosts caps how many host:port targets -scan specs may expand to, so
// a typo like 10.0.0.0/8 cannot turn into a multi-million-host probe loop.
const MaxScanHosts = 4096

// ParseScanTargets expands -scan specs into host:port targets (stable order,
// deduplicated). Accepted forms, IPv4 only:
//
//	192.168.1.50            single host, defaultPort
//	192.168.1.50:5556       single host, explicit port (single IPs only)
//	192.168.1.0/24          CIDR; network and broadcast skipped below /31
//	192.168.1.10-192.168.1.40, 192.168.1.10-40   inclusive range
func ParseScanTargets(specs []string, defaultPort int) ([]string, error) {
	if defaultPort < 1 || defaultPort > 65535 {
		return nil, fmt.Errorf("invalid scan port %d", defaultPort)
	}
	seen := map[string]bool{}
	var out []string
	add := func(a netip.Addr, port int) {
		t := net.JoinHostPort(a.String(), strconv.Itoa(port))
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, spec := range specs {
		spec = strings.TrimSpace(spec)
		first, last, port, err := parseScanSpec(spec, defaultPort)
		if err != nil {
			return nil, fmt.Errorf("scan target %q: %w", spec, err)
		}
		lo, hi := beUint32(first.As4()), beUint32(last.As4())
		if count := int64(hi) - int64(lo) + 1; count > MaxScanHosts {
			return nil, fmt.Errorf("scan target %q: expands to %d hosts, more than the %d limit", spec, count, MaxScanHosts)
		}
		// Counted loop over uint32 (lo <= hi is guaranteed by parseScanSpec),
		// so a bad bound can never make it spin.
		for n := int64(lo); n <= int64(hi); n++ {
			v := uint32(n)
			add(netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), port)
		}
		if len(out) > MaxScanHosts {
			return nil, fmt.Errorf("scan targets expand to %d hosts, more than the %d limit", len(out), MaxScanHosts)
		}
	}
	return out, nil
}

func beUint32(b [4]byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// parseScanSpec returns the inclusive first..last address range of one spec.
func parseScanSpec(spec string, defaultPort int) (first, last netip.Addr, port int, err error) {
	port = defaultPort
	bad := func(e error) (netip.Addr, netip.Addr, int, error) { return netip.Addr{}, netip.Addr{}, 0, e }
	switch {
	case spec == "":
		return bad(errors.New("empty"))
	case strings.Contains(spec, "/"):
		p, e := netip.ParsePrefix(spec)
		if e != nil || !p.Addr().Is4() {
			return bad(errors.New("not an IPv4 CIDR"))
		}
		p = p.Masked()
		first = p.Addr()
		v := beUint32(first.As4()) | uint32(uint64(1)<<(32-p.Bits())-1) // set the host bits
		last = netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
		if p.Bits() < 31 { // /31 and /32 have no network/broadcast address
			first, last = first.Next(), last.Prev()
		}
	case strings.Contains(spec, "-"):
		lo, hi, _ := strings.Cut(spec, "-")
		first, e := netip.ParseAddr(lo)
		if e != nil || !first.Is4() {
			return bad(errors.New("range start is not an IPv4 address"))
		}
		if !strings.Contains(hi, ".") { // short form: replace the last octet
			n, e := strconv.Atoi(hi)
			if e != nil || n < 0 || n > 255 {
				return bad(errors.New("range end is not an octet 0-255"))
			}
			b := first.As4()
			b[3] = byte(n)
			last = netip.AddrFrom4(b)
		} else if last, e = netip.ParseAddr(hi); e != nil || !last.Is4() {
			return bad(errors.New("range end is not an IPv4 address"))
		}
		if last.Less(first) {
			return bad(errors.New("reversed range"))
		}
		return first, last, port, nil
	default:
		if ap, e := netip.ParseAddrPort(spec); e == nil {
			if !ap.Addr().Is4() || ap.Port() == 0 {
				return bad(errors.New("not an IPv4 address:port"))
			}
			return ap.Addr(), ap.Addr(), int(ap.Port()), nil
		}
		a, e := netip.ParseAddr(spec)
		if e != nil || !a.Is4() {
			return bad(errors.New("not an IPv4 address, CIDR or range"))
		}
		return a, a, port, nil
	}
	return first, last, port, nil
}

// scanADB is the part of ADB the Scanner needs; ADB satisfies it.
type scanADB interface {
	Devices(ctx context.Context) ([]Device, error)
	Connect(ctx context.Context, hostPort string) error
	Disconnect(ctx context.Context, hostPort string) error
}

// Scanner finds network-adb devices (Android TV boxes, ...) that the USB
// hot-plug watch cannot see: it probes Targets and runs `adb connect` on the
// ones with the adb port open. Watch then sees the new ip:port serial in
// `adb devices` and provisions it like any other device.
type Scanner struct {
	ADB         scanADB
	Targets     []string // host:port, see ParseScanTargets
	Dial        func(ctx context.Context, network, addr string) (net.Conn, error)
	Concurrency int // parallel probes; default 64
	Log         *slog.Logger
}

const (
	defaultProbeTimeout = 800 * time.Millisecond
	defaultScanConc     = 64
)

// IsScanTarget returns a membership test for targets, for Watch's Include
// filter (adb reports a connected network device as its host:port).
func IsScanTarget(targets []string) func(serial string) bool {
	set := make(map[string]bool, len(targets))
	for _, t := range targets {
		set[t] = true
	}
	return func(serial string) bool { return set[serial] }
}

// ScanOnce probes every target that is not already usable and connects the
// ones with an open adb port. It returns the targets it connected. Errors are
// logged, never fatal: the next round retries.
func (s *Scanner) ScanOnce(ctx context.Context) (connected []string) {
	devs, err := s.ADB.Devices(ctx)
	if err != nil {
		// Without the device list every target would look new; skip the round.
		s.Log.Error("scan: adb devices failed", "err", err)
		return nil
	}
	skip, offline := map[string]bool{}, map[string]bool{}
	for _, d := range devs {
		// unauthorized needs the user to accept the prompt on the TV;
		// reconnecting would not help. offline is retried.
		switch d.State {
		case "device", "unauthorized":
			skip[d.Serial] = true
		case "offline":
			offline[d.Serial] = true
		}
	}
	var todo []string
	for _, t := range s.Targets {
		if !skip[t] {
			todo = append(todo, t)
		}
	}

	dial := s.Dial
	if dial == nil {
		d := net.Dialer{Timeout: defaultProbeTimeout}
		dial = d.DialContext
	}
	conc := s.Concurrency
	if conc <= 0 {
		conc = defaultScanConc
	}
	open := make([]bool, len(todo))
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i, t := range todo {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			c, err := dial(ctx, "tcp", t)
			if err != nil {
				s.Log.Debug("scan: port closed", "target", t, "err", err)
				return
			}
			_ = c.Close()
			open[i] = true
		}()
	}
	wg.Wait()

	for i, t := range todo {
		if !open[i] || ctx.Err() != nil {
			continue
		}
		// adb answers "already connected" to a connect on an offline entry
		// and does nothing, so drop the stale entry first.
		if offline[t] {
			if err := s.ADB.Disconnect(ctx, t); err != nil {
				s.Log.Warn("scan: adb disconnect failed", "target", t, "err", err)
			}
		}
		if err := s.ADB.Connect(ctx, t); err != nil {
			s.Log.Warn("scan: adb connect failed", "target", t, "err", err)
			continue
		}
		s.Log.Info("scan: connected network adb device", "target", t)
		connected = append(connected, t)
	}
	return connected
}

// Run scans immediately and then every interval until ctx is done.
func (s *Scanner) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.ScanOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// WatchInclude builds Watch's Include filter. With no -serial/-connect
// (allowed empty) every device is managed, USB phones included, so -scan
// alone only adds network devices. With an allow-list, scanned serials are
// managed on top of it.
func WatchInclude(allowed map[string]bool, scan func(string) bool) func(string) bool {
	return func(s string) bool {
		return len(allowed) == 0 || allowed[s] || (scan != nil && scan(s))
	}
}
