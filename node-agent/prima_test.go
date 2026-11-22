package nodeagent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dzaczek/phoneborg/proto"
)

// fakePrimaWorker is what the test binary does when PHONEBORG_FAKE_PRIMA
// names a file: it records its arguments there and listens on
// data-port+rank like a prima.cpp worker, until killed (see TestMain).
func fakePrimaWorker() {
	args := os.Args[1:]
	flagInt := func(name string) int {
		for i, a := range args {
			if a == name && i+1 < len(args) {
				n, _ := strconv.Atoi(args[i+1])
				return n
			}
		}
		return 0
	}
	_ = os.WriteFile(os.Getenv("PHONEBORG_FAKE_PRIMA"), []byte(strings.Join(args, " ")), 0o600)
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", flagInt("--data-port")+flagInt("--rank")))
	if err != nil {
		os.Exit(1)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			os.Exit(1)
		}
		c.Close()
	}
}

func TestPrimaPortsAndArgs(t *testing.T) {
	for _, tc := range []struct{ serve, rank, data, signal int }{
		{18090, 1, 18089, 18091},
		{18090, 2, 18088, 18090},
		{18090, 3, 18087, 18089},
	} {
		// The worker binds data+rank: the serving port, which the adb forward exposes.
		if data, signal := primaPorts(tc.serve, tc.rank); data != tc.data || signal != tc.signal || data+tc.rank != tc.serve {
			t.Errorf("rank %d: %d %d", tc.rank, data, signal)
		}
	}
	r := proto.PrimaRank{World: 4, Rank: 2, Epoch: 5, CtxSize: 4096}
	got := strings.Join(primaWorkerArgs("models/big.gguf", 18090, 4, r), " ")
	for _, want := range []string{"-m models/big.gguf", "--world 4 --rank 2", "--master 127.0.0.1 --next 127.0.0.1",
		"--data-port 18088 --signal-port 18090", "-c 4096 -t 4", "--prefetch"} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q lack %q", got, want)
		}
	}
	for _, tc := range []struct {
		rank, world int
		want        map[int]string
	}{
		{1, 3, map[int]string{18089: "master", 18091: "next"}}, // middle: master at serve-1, next at serve+1
		{2, 3, map[int]string{18088: "master"}},                // last: its next is the master
		{1, 2, map[int]string{18089: "master"}},
		{2, 4, map[int]string{18088: "master", 18091: "next"}},
	} {
		if got := primaTunnelPorts(18090, proto.PrimaRank{World: tc.world, Rank: tc.rank}); !maps.Equal(got, tc.want) {
			t.Errorf("rank %d of %d: %v, want %v", tc.rank, tc.world, got, tc.want)
		}
	}
}

// primaTestModel is the "model" fakeController serves at /v1/model-files/big.
var primaTestModel = []byte(strings.Repeat("gguf", 1024))

// fakeController answers PathPrimaDial with an upgrade and then echoes with
// the target as a prefix, and serves primaTestModel; it records the dial
// requests.
func fakeController(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var reqs []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/model-files/big", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "big.gguf", time.Time{}, bytes.NewReader(primaTestModel))
	})
	mux.HandleFunc("GET "+proto.PathPrimaDial, func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, r.PathValue("id")+" "+r.URL.Query().Get("to")+" "+r.Header.Get("Upgrade"))
		if r.URL.Query().Get("to") == "refuse" {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		c, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + proto.UpgradeProtocol + "\r\n\r\n")
		brw.WriteString("hi:") // sent with the response: must not be lost in a buffer
		brw.Flush()
		buf := make([]byte, 64)
		for {
			n, err := brw.Read(buf)
			if err != nil {
				return
			}
			c.Write(append([]byte(r.URL.Query().Get("to")+":"), buf[:n]...))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func TestDialController(t *testing.T) {
	srv, reqs := fakeController(t)
	c, err := dialController(context.Background(), srv.URL, "node 1", "next")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	greeting := make([]byte, 3)
	if _, err := io.ReadFull(c, greeting); err != nil || string(greeting) != "hi:" {
		t.Fatalf("bytes sent with the response: %q %v", greeting, err)
	}
	c.Write([]byte("ping"))
	buf := make([]byte, len("next:ping"))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "next:ping" {
		t.Fatalf("echo: %q %v", buf, err)
	}
	if want := "node 1 next " + proto.UpgradeProtocol; len(*reqs) != 1 || (*reqs)[0] != want {
		t.Fatalf("requests %q, want %q", *reqs, want)
	}
	if _, err := dialController(context.Background(), srv.URL, "n", "refuse"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("refused: %v", err)
	}
	if _, err := dialController(context.Background(), "https://x", "n", "next"); err == nil {
		t.Fatal("https accepted")
	}
}

func TestPrimaTunnel(t *testing.T) {
	srv, _ := fakeController(t)
	p1, p2 := freePort(t), freePort(t)
	dial := func(ctx context.Context, to string) (net.Conn, error) { return dialController(ctx, srv.URL, "n", to) }
	tun, err := startPrimaTunnel(context.Background(), map[int]string{p1: "next", p2: "master"}, dial)
	if err != nil {
		t.Fatal(err)
	}
	for port, to := range map[int]string{p1: "next", p2: "master"} {
		c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		c.Write([]byte("x"))
		want := "hi:" + to + ":x"
		buf := make([]byte, len(want))
		if _, err := io.ReadFull(bufio.NewReader(c), buf); err != nil || string(buf) != want {
			t.Errorf("port %d: %q %v, want %q", port, buf, err, want)
		}
		c.Close()
	}
	// Closing ends listening and live connections.
	live, _ := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p1))
	live.SetDeadline(time.Now().Add(5 * time.Second))
	live.Write([]byte("x"))
	tun.Close()
	if _, err := io.ReadAll(live); err != nil && !strings.Contains(err.Error(), "reset") {
		t.Errorf("live connection: %v", err)
	}
	if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p1)); err == nil {
		c.Close()
		t.Error("still listening after Close")
	}
}

func TestManagerPrimaMode(t *testing.T) {
	t.Chdir(t.TempDir())
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("PHONEBORG_FAKE_PRIMA", argsFile)
	sum := sha256.Sum256(primaTestModel)
	sha, size := hex.EncodeToString(sum[:]), int64(len(primaTestModel))
	ctrl, reqs := fakeController(t)
	modelsDir := t.TempDir()
	servePort := freePortRange(t, -1, 2) // the worker and the tunnel bind servePort-1..servePort+2
	m := NewManager(ManagerConfig{Port: servePort, AdvertisePort: 40123, PrimaWorkerBin: os.Args[0], Threads: 3, ModelsDir: modelsDir,
		ControllerURL: ctrl.URL, NodeID: "pixel"}, discardLog())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &proto.DesiredRuntime{Mode: proto.ModePrima, ModelID: "big", URL: "/v1/model-files/big", SHA256: sha, SizeBytes: size,
		Prima: proto.PrimaRank{World: 3, Rank: 1, Epoch: 7, CtxSize: 2048}}

	// The switch downloads the model first, like any other, then starts the worker.
	m.reconcile(ctx, d)
	st := waitState(t, m, proto.ModePrima)
	if st.Ready || st.PrimaEpoch != 7 || st.Model != "" || st.AdvertisePort != 40123 {
		t.Fatalf("status: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(modelsDir, "big.gguf")); err != nil {
		t.Fatal("model not downloaded:", err)
	}
	args, _ := os.ReadFile(argsFile)
	want := fmt.Sprintf("-m %s --world 3 --rank 1 --master 127.0.0.1 --next 127.0.0.1 --data-port %d --signal-port %d -c 2048 -t 3 --prefetch",
		filepath.Join(modelsDir, "big.gguf"), servePort-1, servePort+1)
	if string(args) != want {
		t.Fatalf("worker args %q, want %q", args, want)
	}
	pid := m.runtime.pid.Load()
	m.reconcile(ctx, d) // unchanged: the same worker keeps running
	if m.runtime.pid.Load() != pid {
		t.Fatal("restarted for an unchanged desired state")
	}

	// The agent listens where the worker sends, and tunnels to the controller.
	for _, tc := range []struct {
		port int
		to   string
	}{{servePort + 1, "next"}, {servePort - 1, "master"}} {
		c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", tc.port))
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		c.Write([]byte("z"))
		want := "hi:" + tc.to + ":z"
		buf := make([]byte, len(want))
		if _, err := io.ReadFull(c, buf); err != nil || string(buf) != want {
			t.Errorf("port %d: %q %v", tc.port, buf, err)
		}
		c.Close()
	}
	if !slices.Contains(*reqs, "pixel next "+proto.UpgradeProtocol) {
		t.Errorf("requests %q", *reqs)
	}

	// A new epoch restarts the worker: the ring restarts as a whole.
	d2 := *d
	d2.Prima.Epoch = 8
	m.reconcile(ctx, &d2)
	st = waitState(t, m, proto.ModePrima)
	if st.PrimaEpoch != 8 || m.runtime.pid.Load() == pid {
		t.Fatalf("after a new epoch: %+v pid %d, was %d", st, m.runtime.pid.Load(), pid)
	}

	// A worker that died and came back on its own is not in the ring.
	if err := syscall.Kill(int(m.runtime.pid.Load()), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); m.runtime.restarts.Load() == 0 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
	}
	time.Sleep(2 * time.Second) // the restarted worker listens again
	if st := m.Status(ctx); st.State == proto.ModePrima || st.Restarts == 0 {
		t.Fatalf("restarted worker reports %+v", st)
	}

	// No desired state leaves prima mode, like RPC mode; nothing ran before.
	m.SetDesired(nil)
	m.reconcile(ctx, <-m.desiredCh)
	if st := m.Status(ctx); st.State != "idle" || m.rpc || m.prima {
		t.Fatalf("after release: %+v", st)
	}
	if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", servePort+1)); err == nil {
		c.Close()
		t.Error("tunnel still listening after leaving prima mode")
	}
}

func TestManagerPrimaWithoutWorker(t *testing.T) {
	m := NewManager(ManagerConfig{Port: freePort(t)}, discardLog())
	m.reconcile(context.Background(), &proto.DesiredRuntime{Mode: proto.ModePrima, ModelID: "big"})
	if st := m.Status(context.Background()); st.State != "error" || !strings.Contains(st.Error, "no prima worker") {
		t.Fatalf("status: %+v", st)
	}
}

func TestManagerPrimaDownloadShowsProgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	m := NewManager(ManagerConfig{Port: freePort(t), PrimaWorkerBin: os.Args[0], ModelsDir: t.TempDir(), ControllerURL: srv.URL}, discardLog())
	m.reconcile(context.Background(), &proto.DesiredRuntime{Mode: proto.ModePrima, ModelID: "big", URL: "/x", SHA256: "abc", SizeBytes: 10})
	// A failed download is an error that names the model, like a normal switch.
	if st := m.Status(context.Background()); st.State != "error" || !strings.Contains(st.Error, "big") || m.prima {
		t.Fatalf("status: %+v", st)
	}
	// While it runs the node reports downloading with the target model.
	m.mu.Lock()
	m.switching, m.subState, m.target, m.progress, m.lastErr = true, "downloading", "big", 0.4, ""
	m.mu.Unlock()
	if st := m.Status(context.Background()); st.State != "downloading" || st.ModelID != "big" || st.Progress != 0.4 {
		t.Fatalf("status: %+v", st)
	}
}

// freePortRange returns a port p such that p+lo..p+hi are all free.
func freePortRange(t *testing.T, lo, hi int) int {
	t.Helper()
	for range 50 {
		p := freePort(t)
		var lns []net.Listener
		ok := true
		for q := p + lo; q <= p+hi; q++ {
			ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(q))
			if err != nil {
				ok = false
				break
			}
			lns = append(lns, ln)
		}
		for _, ln := range lns {
			ln.Close()
		}
		if ok {
			return p
		}
	}
	t.Fatal("no free port range")
	return 0
}
