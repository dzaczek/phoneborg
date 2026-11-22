package controller

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dzaczek/phoneborg/controller/models"
	"github.com/dzaczek/phoneborg/proto"
)

// The prima.cpp backend of Big Brain pools (ADR-037). prima.cpp is a ring:
// rank 0, the head (llama-server on this host), then one llama-cli worker per
// phone, each sending to the next over ZeroMQ. Phones are only reachable
// through their adb forwards, so the controller relays the ring: the head
// sends to a relay that forwards to rank 1, and the workers send through
// their agents' tunnels (PathPrimaDial) to the next phone or to the head.

// Big Brain backends (Pool.Backend).
const (
	BackendRPC   = "rpc" // llama.cpp RPC (ADR-034), the default
	BackendPrima = "prima"
)

// primaPortSpan is how many consecutive head ports a prima head takes: the
// OpenAI API, the ring's data port, the relay's port (data+1, where the head
// sends) and the signal port.
const primaPortSpan = 4

// primaArchs are the architectures prima.cpp supports at the pinned commit.
var primaArchs = []string{"llama", "qwen2"}

// checkPrimaModel reports why prima.cpp cannot run m.
func checkPrimaModel(m models.Model) error {
	if m.Arch != "" && !slices.Contains(primaArchs, m.Arch) {
		return fmt.Errorf("model %q has architecture %q: prima.cpp runs %s only", m.ID, m.Arch, strings.Join(primaArchs, " and "))
	}
	return nil
}

// bigBrainBackend returns p's backend, BackendRPC by default.
func bigBrainBackend(p Pool) string {
	if p.Backend == "" {
		return BackendRPC
	}
	return p.Backend
}

// headBin is the head binary for p's backend; "" = the controller cannot run it.
func (b *bigBrains) headBin(p Pool) string {
	if p.Backend == BackendPrima {
		return b.opts.PrimaHeadBin
	}
	return b.opts.HeadBin
}

// primaRing is a running prima head's ring: the members in rank order (rank
// 1 first) and the ports it uses.
type primaRing struct {
	ids   []string // member node ids
	addrs []string // member ring addresses (their adb forwards)
	// ports, from the head's first allocated port
	headPort, dataPort, relayPort, signalPort int
	conns                                     connSet
	relay                                     net.Listener
}

func newPrimaRing(ids, addrs []string, port int) *primaRing {
	return &primaRing{ids: ids, addrs: addrs, headPort: port, dataPort: port + 1, relayPort: port + 2, signalPort: port + 3}
}

func (r *primaRing) headAddr() string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(r.dataPort)) }

// target is where the connection of member index i (rank i+1) goes: its next
// rank, or the head for the last rank and for the master.
func (r *primaRing) target(i int, to string) (string, bool) {
	switch {
	case to == "master", to == "next" && i == len(r.addrs)-1:
		return r.headAddr(), true
	case to == "next":
		return r.addrs[i+1], true
	}
	return "", false
}

// startRelay forwards the head's sends (the relay port) to rank 1.
func (r *primaRing) startRelay() error {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(r.relayPort)))
	if err != nil {
		return err
	}
	r.relay = ln
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				up, err := net.DialTimeout("tcp", r.addrs[0], 5*time.Second)
				if err != nil {
					c.Close() // the head's ZeroMQ socket reconnects
					return
				}
				r.conns.splice(c, up)
			}()
		}
	}()
	return nil
}

// Close stops the relay and ends every connection of the ring.
func (r *primaRing) Close() {
	if r.relay != nil {
		r.relay.Close()
	}
	r.conns.closeAll()
}

// connSet tracks spliced connections so a stopped ring can end them.
type connSet struct {
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

func (s *connSet) splice(a, b net.Conn) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		a.Close()
		b.Close()
		return
	}
	if s.conns == nil {
		s.conns = map[net.Conn]struct{}{}
	}
	s.conns[a], s.conns[b] = struct{}{}, struct{}{}
	s.mu.Unlock()
	proto.Splice(a, b)
	s.mu.Lock()
	delete(s.conns, a)
	delete(s.conns, b)
	s.mu.Unlock()
}

func (s *connSet) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for c := range s.conns {
		c.Close()
	}
}

// primaWindows splits layers over the ring: rank 0 (the host) takes one
// layer, as little as possible like the RPC head, and the members share the
// rest in proportion to weights (their usable RAM), each at least one. The
// windows sum to layers: prima.cpp cycles through them.
func primaWindows(layers int, weights []int64) ([]int, error) {
	n := len(weights)
	if layers < n+1 {
		return nil, fmt.Errorf("the model has %d layers, fewer than the %d ranks", layers, n+1)
	}
	out := make([]int, n+1)
	out[0] = 1
	for i := 1; i <= n; i++ {
		out[i] = 1
	}
	rest := layers - n - 1
	var total int64
	for _, w := range weights {
		total += max(w, 0)
	}
	if total == 0 {
		total, weights = int64(n), slices.Repeat([]int64{1}, n)
	}
	// Largest remainder: floor of each share, then the leftovers to the
	// biggest fractions.
	frac := make([]int64, n)
	left := rest
	for i, w := range weights {
		share := int64(rest) * max(w, 0)
		out[i+1] += int(share / total)
		left -= int(share / total)
		frac[i] = share % total
	}
	for ; left > 0; left-- {
		best := 0
		for i := range frac {
			if frac[i] > frac[best] {
				best = i
			}
		}
		out[best+1]++
		frac[best] = -1
	}
	return out, nil
}

// primaFit checks that every member can hold its window of the file (plus
// the KV margin) in its usable RAM, and names the first that cannot.
func primaFit(members []bigBrainMember, windows []int, layers int, sizeBytes int64) error {
	for i, mb := range members {
		have := max(int64(mb.freeRAM)-bigBrainReserve, 0)
		need := int64(float64(sizeBytes) * float64(windows[i+1]) / float64(layers) * bigBrainKVMargin)
		if need > have {
			return fmt.Errorf("%s would hold %d of %d layers, needing %.1f GB, but has %.1f GB free: add phones or pick a smaller model",
				mb.name, windows[i+1], layers, float64(need)/1e9, float64(have)/1e9)
		}
	}
	return nil
}

// primaHeadArgs is the head's prima.cpp llama-server command line: rank 0 of
// the ring, sending to the relay at dataPort+1 on this host, with explicit
// layer windows (every window > 0, so prima.cpp neither profiles nor drops
// devices). -fa and -ctk are left out: prima.cpp's -fa takes no value and its
// KV cache types are not the llama.cpp ones.
func primaHeadArgs(model, path string, ctxSize int, r *primaRing, windows []int) []string {
	lw := make([]string, len(windows))
	for i, w := range windows {
		lw[i] = strconv.Itoa(w)
	}
	return []string{"-m", path, "-a", model, "--world", strconv.Itoa(len(windows)), "--rank", "0",
		"--master", "127.0.0.1", "--next", "127.0.0.1",
		"--data-port", strconv.Itoa(r.dataPort), "--signal-port", strconv.Itoa(r.signalPort),
		"-lw", strings.Join(lw, ","), "-c", strconv.Itoa(ctxSize), "-np", "1",
		"--host", "127.0.0.1", "--port", strconv.Itoa(r.headPort)}
}

// nextEpoch is the epoch after e: strictly larger, and at least the clock,
// so a restarted controller never repeats an epoch its workers still run.
func nextEpoch(e int64) int64 { return max(e+1, time.Now().Unix()) }

// epochOf returns pool's current ring epoch, starting one if there is none.
func (b *bigBrains) epochOf(pool string) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := b.heads[pool]
	if h == nil {
		h = &head{}
		b.heads[pool] = h
	}
	if h.epoch == 0 {
		h.epoch = nextEpoch(0)
	}
	return h.epoch
}

// primaTarget resolves where node's connection "to" goes, if node is a
// member of a running prima head.
func (b *bigBrains) primaTarget(node, to string) (target string, conns *connSet, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, h := range b.heads {
		if h.ring == nil || h.cancel == nil || h.exited {
			continue
		}
		if i := slices.Index(h.ring.ids, node); i >= 0 {
			target, ok = h.ring.target(i, to)
			return target, &h.ring.conns, ok
		}
	}
	return "", nil, false
}

// handlePrimaDial is PathPrimaDial: it hijacks the connection of a prima
// worker's agent and splices it to the ring peer the worker would dial.
// Like the heartbeat it carries no token; being a member of a running prima
// pool is what authorises it.
func (s *Server) handlePrimaDial(w http.ResponseWriter, r *http.Request) {
	to := r.URL.Query().Get("to")
	if to != "next" && to != "master" {
		httpError(w, http.StatusBadRequest, "to must be next or master")
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), proto.UpgradeProtocol) {
		httpError(w, http.StatusBadRequest, "want Upgrade: "+proto.UpgradeProtocol)
		return
	}
	target, conns, ok := s.bb.primaTarget(r.PathValue("id"), to)
	if !ok {
		httpError(w, http.StatusForbidden, "not a member of a running prima pool")
		return
	}
	up, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	c, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		up.Close()
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = c.SetDeadline(time.Time{})
	_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + proto.UpgradeProtocol + "\r\n\r\n")
	if err := brw.Flush(); err != nil {
		c.Close()
		up.Close()
		return
	}
	conns.splice(&hijacked{Conn: c, r: brw.Reader}, up)
}

// hijacked reads through the buffer http.Hijacker returns, which may hold
// the first tunnelled bytes.
type hijacked struct {
	net.Conn
	r *bufio.Reader
}

func (h *hijacked) Read(p []byte) (int, error) { return h.r.Read(p) }
