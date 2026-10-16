package nodeagent

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dzaczek/phoneborg/proto"
)

// TestMain doubles as a fake ggml-rpc-server: with PHONEBORG_FAKE_RPC set,
// the test binary listens on its -p port until killed.
func TestMain(m *testing.M) {
	if os.Getenv("PHONEBORG_FAKE_RPC") != "" {
		port := ""
		for i, a := range os.Args {
			if a == "-p" && i+1 < len(os.Args) {
				port = os.Args[i+1]
			}
		}
		l, err := net.Listen("tcp", "127.0.0.1:"+port)
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
	os.Exit(m.Run())
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitState(t *testing.T, m *Manager, want string) *proto.RuntimeStatus {
	t.Helper()
	var st *proto.RuntimeStatus
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if st = m.Status(context.Background()); st.State == want {
			return st
		}
	}
	t.Fatalf("state %q, want %q (error %q)", st.State, want, st.Error)
	return nil
}

func TestManagerRPCMode(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("PHONEBORG_FAKE_RPC", "1")
	cache := filepath.Join(t.TempDir(), "rpc-cache")
	m := NewManager(ManagerConfig{Port: freePort(t), AdvertisePort: 40123, RPCServerBin: os.Args[0], RPCThreads: 4, RPCCacheDir: cache}, discardLog())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A stale cache of another model is wiped.
	os.MkdirAll(cache, 0o755)
	os.WriteFile(filepath.Join(cache, ".model"), []byte("old\n"), 0o644)
	os.WriteFile(filepath.Join(cache, "tensor"), []byte("x"), 0o644)

	d := &proto.DesiredRuntime{Mode: proto.ModeRPC, ModelID: "big"}
	m.reconcile(ctx, d)
	st := waitState(t, m, proto.ModeRPC)
	if st.Ready || st.Model != "" || st.AdvertisePort != 40123 || st.Threads != 4 {
		t.Fatalf("rpc status: %+v", st) // never a backend itself
	}
	if b, _ := os.ReadFile(filepath.Join(cache, ".model")); strings.TrimSpace(string(b)) != "big" {
		t.Fatalf("cache marker %q", b)
	}
	if _, err := os.Stat(filepath.Join(cache, "tensor")); err == nil {
		t.Fatal("stale cache kept")
	}
	pid := m.runtime.pid.Load()
	m.reconcile(ctx, d) // unchanged: keeps the same process
	if m.runtime.pid.Load() != pid {
		t.Fatal("restarted for an unchanged desired state")
	}

	// No desired state while in RPC mode leaves it; nothing ran before.
	m.SetDesired(nil)
	m.reconcile(ctx, <-m.desiredCh)
	if st := m.Status(ctx); st.State != "idle" || m.rpc {
		t.Fatalf("after release: %+v", st)
	}
	// Outside RPC mode nil stays a no-op.
	m.SetDesired(nil)
	select {
	case d := <-m.desiredCh:
		t.Fatalf("queued %+v", d)
	default:
	}
}

func TestManagerRPCWithoutBinary(t *testing.T) {
	m := NewManager(ManagerConfig{Port: freePort(t)}, discardLog())
	m.reconcile(context.Background(), &proto.DesiredRuntime{Mode: proto.ModeRPC, ModelID: "big"})
	if st := m.Status(context.Background()); st.State != "error" || !strings.Contains(st.Error, "no ggml-rpc-server") {
		t.Fatalf("status: %+v", st)
	}
}
