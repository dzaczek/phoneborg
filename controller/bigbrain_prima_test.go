package controller

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dzaczek/phoneborg/controller/models"
	"github.com/dzaczek/phoneborg/controller/models/modeltest"
	"github.com/dzaczek/phoneborg/proto"
)

func TestPrimaWindows(t *testing.T) {
	for _, tc := range []struct {
		layers  int
		weights []int64
		want    []int // nil = error
	}{
		{24, []int64{5244, 2172}, []int{1, 16, 7}},
		{24, []int64{100, 100}, []int{1, 11, 12}},
		{33, []int64{1, 1, 1}, []int{1, 10, 11, 11}},
		{24, []int64{0, 0}, []int{1, 11, 12}},     // no RAM report: even split
		{24, []int64{100000, 1}, []int{1, 22, 1}}, // each at least one layer
		{3, []int64{10, 10}, []int{1, 1, 1}},      // one layer each
		{2, []int64{10, 10}, nil},                 // fewer layers than ranks
	} {
		got, err := primaWindows(tc.layers, tc.weights)
		if tc.want == nil {
			if err == nil {
				t.Errorf("%d layers over %v: %v, want an error", tc.layers, tc.weights, got)
			}
			continue
		}
		sum := 0
		for _, w := range got {
			sum += w
		}
		if err != nil || sum != tc.layers || len(got) != len(tc.weights)+1 {
			t.Errorf("%d layers over %v: %v (%v), sum %d", tc.layers, tc.weights, got, err, sum)
			continue
		}
		// Equal weights may tie either way: compare as multisets of the tail.
		a, b := slices.Clone(got[1:]), slices.Clone(tc.want[1:])
		slices.Sort(a)
		slices.Sort(b)
		if got[0] != 1 || !slices.Equal(a, b) {
			t.Errorf("%d layers over %v: %v, want %v", tc.layers, tc.weights, got, tc.want)
		}
	}
}

func TestPrimaFit(t *testing.T) {
	members := []bigBrainMember{{name: "a", freeRAM: 3<<30 + bigBrainReserve}, {name: "b", freeRAM: 2<<30 + bigBrainReserve}}
	if err := primaFit(members, []int{1, 10, 10}, 21, 3<<30); err != nil {
		t.Fatal(err)
	}
	if err := primaFit(members, []int{1, 10, 10}, 21, 9<<29); err == nil || !strings.Contains(err.Error(), "b would hold 10 of 21 layers") {
		t.Fatalf("err %v", err)
	}
}

func TestPrimaHeadArgs(t *testing.T) {
	r := newPrimaRing([]string{"a", "b"}, []string{"h:1", "h:2"}, 18200)
	got := strings.Join(primaHeadArgs("big", "/m/big.gguf", 4096, r, []int{1, 16, 7}), " ")
	for _, want := range []string{"-m /m/big.gguf -a big", "--world 3 --rank 0", "--master 127.0.0.1 --next 127.0.0.1",
		"--data-port 18201 --signal-port 18203", "-lw 1,16,7", "-c 4096 -np 1", "--host 127.0.0.1 --port 18200"} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q lack %q", got, want)
		}
	}
	// The head sends to dataPort+1, where the relay listens.
	if r.relayPort != r.dataPort+1 {
		t.Errorf("relay port %d, data port %d", r.relayPort, r.dataPort)
	}
	for i, tc := range []struct{ to, want string }{{"next", "h:2"}, {"master", "127.0.0.1:18201"}} {
		if got, ok := r.target(0, tc.to); !ok || got != tc.want {
			t.Errorf("rank 1 %d %s: %q", i, tc.to, got)
		}
	}
	if got, _ := r.target(1, "next"); got != "127.0.0.1:18201" { // the last rank sends to the head
		t.Errorf("last rank next: %q", got)
	}
	if _, ok := r.target(0, "prev"); ok {
		t.Error("unknown target accepted")
	}
}

func TestFreePortSpan(t *testing.T) {
	b := newBigBrains(BigBrainOptions{BasePort: 100}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	b.portFree = func(int) bool { return true }
	b.heads["x"] = &head{cancel: func() {}, ring: &primaRing{}, status: BigBrainStatus{Port: 100}}
	b.heads["y"] = &head{cancel: func() {}, status: BigBrainStatus{Port: 105}}
	b.mu.Lock()
	defer b.mu.Unlock()
	if got := b.freePortLocked("z", 1); got != 104 {
		t.Errorf("one port: %d", got)
	}
	if got := b.freePortLocked("z", primaPortSpan); got != 106 { // 104 is free but 105 is not
		t.Errorf("span: %d", got)
	}
	if got := b.freePortLocked("x", primaPortSpan); got != 100 { // its own ports are free for it
		t.Errorf("own: %d", got)
	}
	// 101 cannot be bound, e.g. in TIME_WAIT after a killed head.
	b.portFree = func(p int) bool { return p != 101 }
	if got := b.freePortLocked("x", primaPortSpan); got != 106 { // 102..105 meets y
		t.Errorf("port not bindable: %d", got)
	}
}

func TestNormPoolPrima(t *testing.T) {
	bb := Pool{Name: "bb", Routing: RoutingBigBrain, Model: "big", Nodes: []string{"a", "b"}}
	for _, tc := range []struct {
		mod func(*Pool)
		err string
		got string
	}{
		{func(p *Pool) { p.Backend = "prima" }, "", "prima"},
		{func(p *Pool) { p.Backend = "rpc" }, "", ""},
		{func(p *Pool) {}, "", ""},
		{func(p *Pool) { p.Backend = "ray" }, "backend must be rpc or prima", ""},
		{func(p *Pool) { p.Backend, p.Spec = "prima", SpecNgram }, "not supported on the prima backend", ""},
		{func(p *Pool) { p.Backend, p.Spec, p.Draft = "prima", SpecDraft, "d" }, "not supported on the prima backend", ""},
		{func(p *Pool) { p.Routing, p.Model, p.Nodes, p.Backend = RoutingSpread, "", nil, "prima" }, "Big Brain pools only", ""},
	} {
		p := bb
		tc.mod(&p)
		got, err := normPool(p)
		switch {
		case tc.err == "" && err != nil:
			t.Errorf("%+v: %v", p, err)
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%+v: error %v, want %q", p, err, tc.err)
		case tc.err == "" && got.Backend != tc.got:
			t.Errorf("backend %q, want %q", got.Backend, tc.got)
		}
	}
}

func TestPrimaPoolValidation(t *testing.T) {
	e, _ := newBigBrainEnv(t)
	body := `{"routing":"bigbrain","model":"big","nodes":["a","b"],"backend":"prima"}`
	// The controller needs the prima head binary.
	if w := e.do(http.MethodPut, "/admin/pools/brain", body, testToken); w.Code != 400 || !strings.Contains(w.Body.String(), "-bigbrain-prima-head") {
		t.Fatalf("no head: %d %s", w.Code, w.Body)
	}
	e.srv.bb.opts.PrimaHeadBin = os.Args[0]
	// prima.cpp at the pinned commit does not know qwen3.
	src := t.TempDir() + "/q3-Q4_K_M.gguf"
	os.WriteFile(src, modeltest.GGUF(modeltest.LlamaLike("qwen3", 24, 14, 2, 896, 32768), 1<<20), 0o600)
	e.admin(http.MethodPost, "/admin/models", `{"source":"file://`+src+`","id":"q3"}`, 202, nil)
	e.waitReady("q3")
	if w := e.do(http.MethodPut, "/admin/pools/brain", strings.Replace(body, `"big"`, `"q3"`, 1), testToken); w.Code != 400 || !strings.Contains(w.Body.String(), "prima.cpp runs llama and qwen2 only") {
		t.Fatalf("qwen3: %d %s", w.Code, w.Body)
	}
	var p Pool
	e.admin(http.MethodPut, "/admin/pools/brain", body, 200, &p)
	if p.Backend != BackendPrima || p.BigBrain.Backend != BackendPrima {
		t.Fatalf("pool: %+v %+v", p, p.BigBrain)
	}
	// The RPC default reports its backend too.
	var rp Pool
	e.admin(http.MethodPut, "/admin/pools/rpcpool", `{"routing":"bigbrain","model":"big","nodes":["c","d"],"backend":"rpc"}`, 200, &rp)
	if rp.Backend != "" || rp.BigBrain.Backend != BackendRPC {
		t.Fatalf("rpc pool: %+v %+v", rp, rp.BigBrain)
	}
}

// primaHeartbeat reports node id as a prima worker listening on port with epoch.
func (e *env) primaHeartbeat(id string, port int, epoch int64) *proto.DesiredRuntime {
	_, d := e.heartbeat(proto.Heartbeat{NodeID: id, RAMAvailBytes: 6 << 30,
		Runtime: &proto.RuntimeStatus{State: proto.ModePrima, PrimaEpoch: epoch, AdvertisePort: port}})
	return d
}

func newPrimaEnv(t *testing.T) *env {
	e, _ := newBigBrainEnv(t)
	e.srv.bb.opts.PrimaHeadBin = os.Args[0]
	e.admin(http.MethodPut, "/admin/pools/brain", `{"routing":"bigbrain","model":"big","nodes":["a","b"],"backend":"prima"}`, 200, nil)
	return e
}

func TestPrimaDesiredAndEpoch(t *testing.T) {
	e := newPrimaEnv(t)
	argsFile := os.Getenv("PHONEBORG_FAKE_HEAD")
	_, big, _ := e.srv.catalog.File("big")

	// Every member gets the model download, its rank and the ring's epoch.
	da := e.primaHeartbeat("a", 50052, 0)
	db := e.primaHeartbeat("b", 50053, 0)
	if da == nil || da.Mode != proto.ModePrima || da.ModelID != "big" || da.URL != PathModelFiles+"big" || da.SHA256 != big.SHA256 ||
		da.SizeBytes != big.SizeBytes || da.Prima.World != 3 || da.Prima.Rank != 1 || da.Prima.CtxSize != bigBrainDefaultCtx || da.Prima.Epoch == 0 {
		t.Fatalf("desired for a: %+v", da)
	}
	if db.Prima.Rank != 2 || db.Prima.Epoch != da.Prima.Epoch {
		t.Fatalf("desired for b: %+v", db)
	}
	epoch := da.Prima.Epoch

	// Downloading members are waited for, by what they report.
	e.heartbeat(proto.Heartbeat{NodeID: "a", RAMAvailBytes: 6 << 30, Runtime: &proto.RuntimeStatus{State: "downloading", ModelID: "big", AdvertisePort: 50052}})
	p := e.tickUntil("brain", BigBrainPreparing)
	if !strings.Contains(p.BigBrain.Reason, "a is downloading") || p.Members[0].Reason != ReasonNotPrima+" (downloading)" {
		t.Fatalf("pool: %+v %+v", p.BigBrain, p.Members)
	}
	// A member still on an old epoch does not count.
	e.primaHeartbeat("a", 50052, epoch-1)
	e.primaHeartbeat("b", 50053, epoch)
	p = e.tickUntil("brain", BigBrainPreparing)
	if !strings.Contains(p.BigBrain.Reason, "a is restarting") {
		t.Fatalf("reason %q", p.BigBrain.Reason)
	}

	// All on the current epoch: the head starts with windows summing to the
	// model's 24 layers, the host taking one.
	e.primaHeartbeat("a", 50052, epoch)
	p = e.tickUntil("brain", BigBrainReady)
	if !slices.Equal(p.BigBrain.LayerWindows, []int{1, 12, 11}) || p.BigBrain.Backend != BackendPrima || p.BigBrain.TensorSplit != nil {
		t.Fatalf("status: %+v", p.BigBrain)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--world 3 --rank 0") || !strings.Contains(string(args), "-lw 1,12,11") || strings.Contains(string(args), "--rpc") {
		t.Fatalf("head args %q", args)
	}
	if got := e.srv.bb.epochOf("brain"); got != epoch {
		t.Fatalf("epoch moved to %d while running", got)
	}

	// A member leaving prima mode stops the head and moves the epoch, so the
	// whole ring restarts; the head only comes back when all report it.
	e.heartbeat(proto.Heartbeat{NodeID: "b", RAMAvailBytes: 6 << 30, Runtime: &proto.RuntimeStatus{State: "loading", AdvertisePort: 50053}})
	e.tickUntil("brain", BigBrainPreparing)
	next := e.srv.bb.epochOf("brain")
	e.srv.bb.mu.Lock()
	e.srv.bb.heads["brain"].stopped = time.Time{} // do not wait out the settle time
	e.srv.bb.mu.Unlock()
	if next <= epoch {
		t.Fatalf("epoch %d did not move past %d", next, epoch)
	}
	e.tickUntil("brain", BigBrainPreparing) // not bumped again while waiting
	if got := e.srv.bb.epochOf("brain"); got != next {
		t.Fatalf("epoch bumped while waiting: %d, was %d", got, next)
	}
	if d := e.primaHeartbeat("a", 50052, epoch); d.Prima.Epoch != next {
		t.Fatalf("desired after bump: %+v", d)
	}
	e.primaHeartbeat("a", 50052, next)
	e.primaHeartbeat("b", 50053, next)
	e.tickUntil("brain", BigBrainReady)
}

func TestPrimaHeadExitBumpsEpoch(t *testing.T) {
	bin, err := exec.LookPath("false")
	if err != nil {
		t.Skip("no false binary")
	}
	e := newPrimaEnv(t)
	e.srv.bb.opts.PrimaHeadBin = bin
	epoch := e.primaHeartbeat("a", 50052, 0).Prima.Epoch
	e.primaHeartbeat("b", 50053, epoch)
	e.primaHeartbeat("a", 50052, epoch)
	e.srv.bigBrainTick(context.Background())
	e.srv.bb.mu.Lock()
	ring := e.srv.bb.heads["brain"].ring
	e.srv.bb.mu.Unlock()
	for deadline := time.Now().Add(5 * time.Second); e.srv.bb.epochOf("brain") == epoch && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	if got := e.srv.bb.epochOf("brain"); got <= epoch {
		t.Fatalf("epoch %d after the head exited, was %d", got, epoch)
	}
	// The relay is gone with the head, and the members restart.
	if p := e.tickUntil("brain", BigBrainPreparing); !strings.Contains(p.BigBrain.Reason, "a is restarting") || p.BigBrain.Restarts != 1 {
		t.Fatalf("pool: %+v", p.BigBrain)
	}
	if _, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", ring.relayPort)); err == nil {
		t.Error("relay still listening after the head exited")
	}
}

// echoServer accepts connections on port and sends back what it reads,
// prefixed by tag.
func echoServer(t *testing.T, port int, tag string) {
	t.Helper()
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 64)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					c.Write(append([]byte(tag), buf[:n]...))
				}
			}()
		}
	}()
}

// dialUpgrade makes the agent's request against srv and returns the status
// and the connection, read through the buffer that parsed the response.
func dialUpgrade(t *testing.T, srv *httptest.Server, node, query string) (int, net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	fmt.Fprintf(c, "GET /v0/nodes/%s/prima-dial?%s HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", node, query, proto.UpgradeProtocol)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, c, br
}

func TestPrimaDialTunnel(t *testing.T) {
	e := newPrimaEnv(t)
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	portA, portB := freeTCPPort(t), freeTCPPort(t)
	echoServer(t, portA, "A:")
	echoServer(t, portB, "B:")
	e.srv.bb.opts.BasePort = freeTCPPort(t) // the head's ports must not meet the echo servers'

	// Without a running ring nobody may dial.
	if code, _, _ := dialUpgrade(t, srv, "a", "to=next"); code != http.StatusForbidden {
		t.Fatalf("before the ring runs: %d", code)
	}
	epoch := e.primaHeartbeat("a", portA, 0).Prima.Epoch
	e.primaHeartbeat("b", portB, epoch)
	e.primaHeartbeat("a", portA, epoch)
	e.tickUntil("brain", BigBrainReady)
	e.srv.bb.mu.Lock()
	ring := e.srv.bb.heads["brain"].ring
	e.srv.bb.mu.Unlock()
	echoServer(t, ring.dataPort, "H:")

	roundTrip := func(c net.Conn, br *bufio.Reader, msg string) string {
		t.Helper()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, len(msg)+2)
		if _, err := io.ReadFull(br, buf); err != nil {
			t.Fatal(err)
		}
		return string(buf)
	}
	// Bytes go both ways: rank 1's next is rank 2, rank 2's next is the head,
	// and everybody's master is the head.
	for _, tc := range []struct{ node, query, want string }{
		{"a", "to=next", "B:hello"},
		{"b", "to=next", "H:hello"},
		{"a", "to=master", "H:hello"},
		{"b", "to=master", "H:hello"},
	} {
		code, c, br := dialUpgrade(t, srv, tc.node, tc.query)
		if code != http.StatusSwitchingProtocols {
			t.Fatalf("%s %s: %d", tc.node, tc.query, code)
		}
		if got := roundTrip(c, br, "hello"); got != tc.want {
			t.Errorf("%s %s: %q, want %q", tc.node, tc.query, got, tc.want)
		}
	}
	// The head's sends reach rank 1 through the relay.
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", ring.relayPort))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := roundTrip(c, bufio.NewReader(c), "head"); got != "A:head" {
		t.Errorf("relay: %q", got)
	}

	// Rejections: a phone outside the pool, a bad target, no upgrade.
	if code, _, _ := dialUpgrade(t, srv, "c", "to=next"); code != http.StatusForbidden {
		t.Errorf("non-member: %d", code)
	}
	if code, _, _ := dialUpgrade(t, srv, "a", "to=prev"); code != http.StatusBadRequest {
		t.Errorf("bad target: %d", code)
	}
	if w := e.do(http.MethodGet, "/v0/nodes/a/prima-dial?to=next", "", ""); w.Code != http.StatusBadRequest {
		t.Errorf("no upgrade: %d", w.Code)
	}

	// Stopping the ring ends its tunnels and refuses new ones.
	code, tc, tbr := dialUpgrade(t, srv, "a", "to=next")
	if code != http.StatusSwitchingProtocols {
		t.Fatal(code)
	}
	e.srv.bb.stop("brain", false)
	tc.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := tbr.ReadByte(); err == nil {
		t.Error("tunnel survived the ring")
	}
	if code, _, _ := dialUpgrade(t, srv, "a", "to=next"); code != http.StatusForbidden {
		t.Errorf("after stop: %d", code)
	}
}

func TestPrimaWithoutLayersOrArch(t *testing.T) {
	m := models.Model{ID: "g", Arch: "gemma3"}
	if err := checkPrimaModel(m); err == nil || !strings.Contains(err.Error(), "llama and qwen2") {
		t.Errorf("gemma3: %v", err)
	}
	for _, arch := range []string{"llama", "qwen2", ""} {
		if err := checkPrimaModel(models.Model{Arch: arch}); err != nil {
			t.Errorf("%q: %v", arch, err)
		}
	}
}
