package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dzaczek/phoneborg/controller/gateway"
	"github.com/dzaczek/phoneborg/controller/models"
	"github.com/dzaczek/phoneborg/proto"
)

// TestMain doubles as a fake Big Brain head: with PHONEBORG_FAKE_HEAD set to
// a file, the test binary writes its arguments there and serves /health and
// chat completions on its --port until killed.
func TestMain(m *testing.M) {
	if f := os.Getenv("PHONEBORG_FAKE_HEAD"); f != "" {
		os.WriteFile(f, []byte(strings.Join(os.Args[1:], " ")), 0o600)
		port := ""
		for i, a := range os.Args {
			if a == "--port" && i+1 < len(os.Args) {
				port = os.Args[i+1]
			}
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"status":"ok"}`) })
		mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"content":"big brain"}}],"usage":{"prompt_tokens":5,"completion_tokens":3},"timings":{"prompt_per_second":9.5,"predicted_per_second":2.5}}`)
		})
		_ = http.ListenAndServe("127.0.0.1:"+port, mux)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestNormPoolBigBrain(t *testing.T) {
	for _, tc := range []struct {
		p   Pool
		err string
	}{
		{Pool{Name: "bb", Routing: RoutingBigBrain, Model: " big ", Nodes: []string{"a", "b"}}, ""},
		{Pool{Name: "bb", Routing: RoutingBigBrain, Model: "big", Nodes: []string{"a", "b"}, CtxSize: 8192}, ""},
		{Pool{Name: "bb", Routing: RoutingBigBrain, Nodes: []string{"a", "b"}}, "needs a model"},
		{Pool{Name: "bb", Routing: RoutingBigBrain, Model: "big", Nodes: []string{"a", "a"}}, "at least 2 nodes"},
		{Pool{Name: "bb", Routing: RoutingBigBrain, Model: "big", Nodes: []string{"a", "b"}, Models: []string{"x"}}, "do not apply"},
		{Pool{Name: "bb", Routing: RoutingBigBrain, Model: "big", Nodes: []string{"a", "b"}, CtxSize: 100}, "ctx_size"},
		{Pool{Name: "bb", Routing: "giant"}, `"bigbrain"`},
	} {
		p, err := normPool(tc.p)
		switch {
		case tc.err == "" && err != nil:
			t.Errorf("%+v: %v", tc.p, err)
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%+v: error %v, want %q", tc.p, err, tc.err)
		case tc.err == "" && p.Model != "big":
			t.Errorf("model %q not trimmed", p.Model)
		}
	}
}

func TestHeadArgs(t *testing.T) {
	got := strings.Join(headArgs("big", "/m/big.gguf", 4096, 18200, []string{"127.0.0.1:1", "127.0.0.1:2"}, []int{7000, 3000}), " ")
	for _, want := range []string{"-m /m/big.gguf -a big", "--rpc 127.0.0.1:1,127.0.0.1:2", "-ngl 99", "-ts 7000,3000", "-c 4096 -np 1",
		"-ctk q8_0 -ctv q8_0", "--host 127.0.0.1 --port 18200"} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q lack %q", got, want)
		}
	}
	have, split := bigBrainSplit([]bigBrainMember{{freeRAM: 8 << 30}, {freeRAM: 500 << 20}})
	if want := int64(8<<30 - bigBrainReserve); have != want || !slices.Equal(split, []int{8192 - 900, 0}) {
		t.Errorf("split: %d %v", have, split)
	}
}

// newBigBrainEnv is a controller with a catalog holding model "big", a
// fake head binary, and nodes a, b, c serving model "m".
func newBigBrainEnv(t *testing.T) (*env, string) {
	return newBigBrainEnvTimes(t, time.Hour, time.Hour)
}

func newBigBrainEnvTimes(t *testing.T, suspect, offline time.Duration) (*env, string) {
	t.Helper()
	dir := t.TempDir()
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	cat, err := models.NewCatalog(models.CatalogOptions{Dir: filepath.Join(dir, "models"), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cat.Close)
	reg := NewRegistry(suspect, offline, log)
	srv := NewServer(reg, time.Second, GatewayOptions{BackendHost: "127.0.0.1",
		Models:   ModelOptions{Catalog: cat},
		BigBrain: BigBrainOptions{HeadBin: os.Args[0], BasePort: freeTCPPort(t), LogDir: filepath.Join(dir, "bigbrain")},
		Config:   gateway.Config{UpstreamTimeout: 5 * time.Second, MaxAttempts: 2, Cooldown: time.Minute}},
		AdminOptions{Token: testToken}, log)
	e := &env{t: t, reg: reg, srv: srv, h: srv.Handler(), logs: logs}
	args := filepath.Join(dir, "head-args")
	t.Setenv("PHONEBORG_FAKE_HEAD", args)
	src := filepath.Join(dir, "Big-Q4_K_M.gguf")
	os.WriteFile(src, ggufFile, 0o600)
	e.admin(http.MethodPost, "/admin/models", `{"source":"file://`+src+`","id":"big"}`, 202, nil)
	if m := e.waitReady("big"); m.Status != models.StatusReady {
		t.Fatalf("model: %+v", m)
	}
	for _, id := range []string{"a", "b", "c"} {
		reg.Register(proto.RegisterRequest{NodeID: id, Inventory: proto.Inventory{RAMTotalBytes: 8 * models.GiB}}, "x")
		_ = reg.ReportBenchmark(proto.BenchmarkReport{NodeID: id})
		e.heartbeat(proto.Heartbeat{NodeID: id, RAMAvailBytes: 4 << 30, Runtime: &proto.RuntimeStatus{Model: "m", Ready: true, AdvertisePort: 1}})
	}
	t.Cleanup(srv.bb.stopAll)
	return e, args
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// rpcHeartbeat reports node id as a running ggml-rpc-server on port.
func (e *env) rpcHeartbeat(id string, port int, free uint64) *proto.DesiredRuntime {
	_, d := e.heartbeat(proto.Heartbeat{NodeID: id, RAMAvailBytes: free, Runtime: &proto.RuntimeStatus{State: proto.ModeRPC, AdvertisePort: port}})
	return d
}

func (e *env) bigBrain(name string) Pool {
	var ps Pools
	e.admin(http.MethodGet, "/admin/pools", "", 200, &ps)
	for _, p := range ps.Pools {
		if p.Name == name {
			return p
		}
	}
	e.t.Fatalf("no pool %s", name)
	return Pool{}
}

// tickUntil runs the supervisor until pool reaches state.
func (e *env) tickUntil(name, state string) Pool {
	e.t.Helper()
	var p Pool
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		e.srv.bigBrainTick(context.Background())
		if p = e.bigBrain(name); p.BigBrain.State == state {
			return p
		}
	}
	e.t.Fatalf("pool %s: %+v, want state %s", name, p.BigBrain, state)
	return p
}

func TestBigBrainPool(t *testing.T) {
	e, argsFile := newBigBrainEnv(t)

	e.admin(http.MethodPut, "/admin/pools/brain", `{"routing":"bigbrain","model":"nope","nodes":["a","b"]}`, 400, nil)
	var p Pool
	e.admin(http.MethodPut, "/admin/pools/brain", `{"routing":"bigbrain","model":"big","nodes":["a","b"]}`, 200, &p)
	if p.BigBrain == nil || p.Status != PoolNoReadyNode || len(p.Members) != 2 || p.Members[0].Eligible {
		t.Fatalf("new pool: %+v", p)
	}
	// A phone joins one Big Brain at a time.
	if w := e.do(http.MethodPut, "/admin/pools/rival", `{"routing":"bigbrain","model":"big","nodes":["b","c"]}`, testToken); w.Code != 409 {
		t.Fatalf("overlap: %d %s", w.Code, w.Body)
	}

	// Members are told to run ggml-rpc-server and leave the planner; c is not.
	if _, d := e.heartbeat(proto.Heartbeat{NodeID: "a", RAMAvailBytes: 4 << 30}); d == nil || d.Mode != proto.ModeRPC || d.ModelID != "big" {
		t.Fatalf("desired for a: %+v", d)
	}
	if _, d := e.heartbeat(proto.Heartbeat{NodeID: "c", RAMAvailBytes: 4 << 30}); d != nil && d.Mode == proto.ModeRPC {
		t.Fatalf("desired for c: %+v", d)
	}
	nodes, _, _ := e.srv.planInputs(nil)
	if len(nodes) != 1 || nodes[0].ID != "c" {
		t.Fatalf("planned nodes: %+v", nodes)
	}

	// The head waits for every member.
	e.rpcHeartbeat("a", 50052, 6<<30)
	p = e.tickUntil("brain", BigBrainPreparing)
	if !strings.Contains(p.BigBrain.Reason, "b is serving") {
		t.Fatalf("reason %q", p.BigBrain.Reason)
	}
	e.rpcHeartbeat("b", 50053, 3<<30)
	p = e.tickUntil("brain", BigBrainReady)
	if p.Status != PoolReady || p.BigBrain.Port == 0 || !p.Members[0].Eligible || !p.Members[1].Eligible {
		t.Fatalf("ready pool: %+v", p)
	}
	args, _ := os.ReadFile(argsFile)
	if want := fmt.Sprintf("--rpc 127.0.0.1:50052,127.0.0.1:50053 -ngl 99 -ts %d,%d", 6*1024-900, 3*1024-900); !strings.Contains(string(args), want) {
		t.Fatalf("head args %q lack %q", args, want)
	}

	// Only pool/brain reaches the head: not auto, not the model name.
	if w := e.chatModel("pool/brain", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "big brain") {
		t.Fatalf("pool/brain: %d %s", w.Code, w.Body)
	}
	if w := e.chatModel("big", ""); w.Code == 200 {
		t.Fatalf("model name reached the head: %s", w.Body)
	}
	if w := e.chatModel("auto", ""); strings.Contains(w.Body.String(), "big brain") {
		t.Fatal("auto reached the head")
	}
	models := e.do(http.MethodGet, "/v1/models", "", "").Body.String()
	if !strings.Contains(models, `"id":"pool/brain"`) || strings.Contains(models, `"id":"big"`) {
		t.Errorf("/v1/models: %s", models)
	}

	// A member leaving RPC mode stops the head.
	e.heartbeat(proto.Heartbeat{NodeID: "b", RAMAvailBytes: 4 << 30, Runtime: &proto.RuntimeStatus{State: "loading"}})
	e.tickUntil("brain", BigBrainPreparing)
	if w := e.chatModel("pool/brain", ""); w.Code == 200 {
		t.Fatal("stopped head still served")
	}

	// Disabling the pool releases its members.
	e.admin(http.MethodPut, "/admin/pools/brain", `{"routing":"bigbrain","model":"big","nodes":["a","b"],"disabled":true}`, 200, nil)
	if d := e.rpcHeartbeat("a", 50052, 6<<30); d != nil && d.Mode == proto.ModeRPC {
		t.Fatalf("disabled pool still claims a: %+v", d)
	}
	e.srv.bigBrainTick(context.Background())
	if p := e.bigBrain("brain"); p.Status != PoolDisabled || p.BigBrain.State != BigBrainStopped {
		t.Fatalf("disabled: %+v", p)
	}
}

func TestBigBrainDoesNotFit(t *testing.T) {
	e, _ := newBigBrainEnv(t)
	e.admin(http.MethodPut, "/admin/pools/brain", `{"routing":"bigbrain","model":"big","nodes":["a","b"]}`, 200, nil)
	e.rpcHeartbeat("a", 50052, bigBrainReserve)
	e.rpcHeartbeat("b", 50053, bigBrainReserve)
	p := e.tickUntil("brain", BigBrainError)
	if !strings.Contains(p.BigBrain.Reason, "needs") || p.BigBrain.HaveBytes != 0 || p.BigBrain.NeedBytes == 0 {
		t.Fatalf("does not fit: %+v", p.BigBrain)
	}
	var b bytes.Buffer
	json.NewEncoder(&b).Encode(p)
	if !strings.Contains(b.String(), `"big_brain":{"state":"error"`) {
		t.Errorf("JSON: %s", b.String())
	}
}

// Loading pushes gigabytes over each phone's USB link and delays its
// heartbeats: a running head keeps members that turn SUSPECT or OFFLINE
// (its RPC connection fails if a phone is really gone), but a head does
// not start without fresh heartbeats.
func TestBigBrainKeepsStaleMembers(t *testing.T) {
	e, _ := newBigBrainEnvTimes(t, 100*time.Millisecond, 200*time.Millisecond)
	e.admin(http.MethodPut, "/admin/pools/brain", `{"routing":"bigbrain","model":"big","nodes":["a","b"]}`, 200, nil)
	e.rpcHeartbeat("a", 50052, 4<<30)
	e.rpcHeartbeat("b", 50053, 4<<30)
	p := e.tickUntil("brain", BigBrainReady)
	port := p.BigBrain.Port

	time.Sleep(300 * time.Millisecond)
	e.reg.Sweep()
	e.srv.bigBrainTick(context.Background())
	if p := e.bigBrain("brain"); p.BigBrain.State != BigBrainReady || p.BigBrain.Port != port || p.Members[0].Reason != "offline" {
		t.Fatalf("stale members stopped the head: %+v %+v", p.BigBrain, p.Members)
	}

	e.srv.bb.stop("brain", false) // e.g. the head exited
	p = e.tickUntil("brain", BigBrainPreparing)
	if !strings.Contains(p.BigBrain.Reason, "a is offline") {
		t.Fatalf("reason %q", p.BigBrain.Reason)
	}
	// Back, but their RAM reports still show the old load for a while.
	e.rpcHeartbeat("a", 50052, 1<<30)
	e.rpcHeartbeat("b", 50053, 1<<30)
	e.srv.bigBrainTick(context.Background())
	if p := e.bigBrain("brain"); p.BigBrain.State != BigBrainPreparing || !strings.Contains(p.BigBrain.Reason, "free the previous load") {
		t.Fatalf("settling: %+v", p.BigBrain)
	}
}
