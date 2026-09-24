package nodeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dzaczek/phoneborg/proto"
)

func TestManagerSetDesiredKeepsOnlyLatest(t *testing.T) {
	m := NewManager(ManagerConfig{}, discardLog())
	d1 := &proto.DesiredRuntime{ModelID: "a"}
	d2 := &proto.DesiredRuntime{ModelID: "b"}
	m.SetDesired(d1)
	m.SetDesired(d2)  // must replace d1, not queue behind it
	m.SetDesired(nil) // must be a no-op

	got := <-m.desiredCh
	if got != d2 {
		t.Fatalf("got %+v, want the latest desired (%+v)", got, d2)
	}
	select {
	case extra := <-m.desiredCh:
		t.Fatalf("unexpected extra desired state: %+v", extra)
	default:
	}
}

func TestManagerReconcileSkipsUnchangedDesired(t *testing.T) {
	m := NewManager(ManagerConfig{ModelsDir: "/nonexistent/impossible/path"}, discardLog())
	d := &proto.DesiredRuntime{ModelID: "m1", SHA256: "abc", SizeBytes: 100}
	m.lastApplied = d

	// Reconciling the same desired state again must be a no-op: it must not
	// touch ModelsDir (which does not exist and would error on statfs).
	m.reconcile(context.Background(), d)

	if m.lastErr != "" {
		t.Fatalf("lastErr = %q, want empty (should have skipped)", m.lastErr)
	}
}

// TestManagerStatusReportsBudget checks that Status reports a memory budget
// (ADR-012) even before any model is loaded, so the controller sees a node's
// real headroom from its very first heartbeat.
func TestManagerStatusReportsBudget(t *testing.T) {
	m := NewManager(ManagerConfig{MemReserveMB: 100}, discardLog())
	st := m.Status(context.Background())
	want := MemoryBudget(AvailableRAM(), 0, 100*mib)
	if st.BudgetBytes != want {
		t.Fatalf("BudgetBytes = %d, want %d", st.BudgetBytes, want)
	}
}

func TestManagerReconcileFailureSetsErrorState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	m := NewManager(ManagerConfig{ModelsDir: dir, ControllerURL: srv.URL}, discardLog())
	d := &proto.DesiredRuntime{ModelID: "missing-model", URL: "/v1/model-files/missing-model", SHA256: "abc", SizeBytes: 100}

	m.reconcile(context.Background(), d)

	st := m.Status(context.Background())
	if st.State != "error" {
		t.Fatalf("State = %q, want %q", st.State, "error")
	}
	if !strings.Contains(st.Error, "missing-model") {
		t.Fatalf("Error = %q, want it to name the model", st.Error)
	}
	if m.lastApplied != nil {
		t.Fatalf("lastApplied = %+v, want nil after a failed switch", m.lastApplied)
	}
}

// TestManagerKeepsRunningModelWhenSettingsDoNotFit covers a node that already
// serves the desired model (started by pcprov -model) when the controller's
// requested settings exceed the memory budget: the agent keeps serving and
// does not report an error.
func TestManagerKeepsRunningModelWhenSettingsDoNotFit(t *testing.T) {
	dir := t.TempDir()
	content := []byte("gguf-bytes")
	sum := sha256.Sum256(content)
	if err := os.WriteFile(filepath.Join(dir, "m1.gguf"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	// A reserve far above any real MemAvailable makes every plan fail.
	m := NewManager(ManagerConfig{ModelsDir: dir, MemReserveMB: 1 << 30}, discardLog())
	m.runtime, m.current = &Runtime{}, "m1"
	d := &proto.DesiredRuntime{ModelID: "m1", SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(content)),
		CtxSize: 16384, Slots: 1, KVType: "auto", Layers: 24, KVHeads: 2, HeadDim: 64}

	m.reconcile(context.Background(), d)

	if m.lastErr != "" {
		t.Fatalf("lastErr = %q, want empty (running model kept)", m.lastErr)
	}
	if m.lastApplied != d {
		t.Fatal("lastApplied not set: the agent would retry on every heartbeat")
	}

	// A different model that does not fit is still an error.
	m.lastApplied = nil
	d2 := *d
	d2.ModelID = "m2"
	os.WriteFile(filepath.Join(dir, "m2.gguf"), content, 0o644)
	m.reconcile(context.Background(), &d2)
	if !strings.Contains(m.lastErr, "m2") {
		t.Fatalf("lastErr = %q, want an error for the non-running model", m.lastErr)
	}
}

// TestManagerStatusReportsResidentBytes covers ADR-012's addendum: the
// Manager's tracked resident bytes (of whatever model is currently running)
// are reported on RuntimeStatus, so the controller's bandwidth estimate
// (ADR-015) can use them.
func TestManagerStatusReportsResidentBytes(t *testing.T) {
	m := NewManager(ManagerConfig{}, discardLog())
	m.mu.Lock()
	m.residentBytes = 123
	m.mu.Unlock()
	if got := m.Status(context.Background()).ResidentBytes; got != 123 {
		t.Fatalf("ResidentBytes = %d, want 123", got)
	}
}

// TestManagerTracksResidentBytesWhenKeepingRunningModel covers the same
// "requested settings do not fit, keep serving" path as
// TestManagerKeepsRunningModelWhenSettingsDoNotFit, checking that it also
// records the desired state's resident bytes for reporting.
func TestManagerTracksResidentBytesWhenKeepingRunningModel(t *testing.T) {
	dir := t.TempDir()
	content := []byte("gguf-bytes")
	sum := sha256.Sum256(content)
	if err := os.WriteFile(filepath.Join(dir, "m1.gguf"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(ManagerConfig{ModelsDir: dir, MemReserveMB: 1 << 30}, discardLog())
	m.runtime, m.current = &Runtime{}, "m1"
	d := &proto.DesiredRuntime{ModelID: "m1", SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(content)),
		ResidentBytes: 5, CtxSize: 16384, Slots: 1, KVType: "auto", Layers: 24, KVHeads: 2, HeadDim: 64}

	m.reconcile(context.Background(), d)

	m.mu.Lock()
	got := m.residentBytes
	m.mu.Unlock()
	if got != 5 {
		t.Fatalf("residentBytes = %d, want 5", got)
	}
}

func TestShouldRetryNow(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name             string
		retryAt          time.Time
		lastFailedBudget int64
		budget           int64
		want             bool
	}{
		{"backoff window elapsed", now.Add(-time.Second), 1000, 1000, true},
		{"still within window, budget unchanged", now.Add(time.Minute), 1000, 1000, false},
		{"still within window, budget grew but not enough", now.Add(time.Minute), 1000, 1099, false},
		{"still within window, budget grew by more than 10%", now.Add(time.Minute), 1000, 1101, true},
		{"still within window, no baseline budget recorded", now.Add(time.Minute), 0, 1_000_000, false},
		{"still within window, budget shrank", now.Add(time.Minute), 1000, 500, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRetryNow(now, tc.retryAt, tc.lastFailedBudget, tc.budget); got != tc.want {
				t.Errorf("shouldRetryNow(...) = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestManagerBacksOffRetriesOfSameFailedDesired covers the coordinator's
// observation on a real Mi 8: a failed switch was retried on every heartbeat
// (every 5 s), re-downloading/re-hashing and re-failing sizing each time.
// The Manager must instead back off (30 s, doubling) between retries of the
// same failed desired state, while still reporting the error the whole time,
// and retry immediately once the state changes or the window elapses.
func TestManagerBacksOffRetriesOfSameFailedDesired(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	m := NewManager(ManagerConfig{ModelsDir: dir, ControllerURL: srv.URL}, discardLog())
	d := &proto.DesiredRuntime{ModelID: "missing-model", URL: "/v1/model-files/missing-model", SHA256: "abc", SizeBytes: 100}

	m.reconcile(context.Background(), d)
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
	if m.backoff != minBackoff {
		t.Fatalf("backoff = %v, want %v", m.backoff, minBackoff)
	}

	// Same desired state, still within the backoff window: must not retry,
	// but Status must keep reporting the error the whole time.
	m.reconcile(context.Background(), d)
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want still 1 (should have backed off)", hits.Load())
	}
	st := m.Status(context.Background())
	if st.State != "error" || !strings.Contains(st.Error, "missing-model") {
		t.Fatalf("status during backoff = %+v", st)
	}

	// Once the backoff window elapses, it retries and doubles the backoff.
	m.mu.Lock()
	m.retryAt = time.Now().Add(-time.Second)
	m.mu.Unlock()
	m.reconcile(context.Background(), d)
	if hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2 after the backoff elapsed", hits.Load())
	}
	if m.backoff != 2*minBackoff {
		t.Fatalf("backoff = %v, want %v", m.backoff, 2*minBackoff)
	}

	// A different desired state is not held back by the old one's backoff.
	d2 := &proto.DesiredRuntime{ModelID: "another-model", URL: "/v1/model-files/another-model", SHA256: "abc", SizeBytes: 100}
	m.reconcile(context.Background(), d2)
	if hits.Load() != 3 {
		t.Fatalf("hits = %d, want 3 for a different desired state", hits.Load())
	}
	if m.backoff != minBackoff {
		t.Fatalf("backoff = %v, want reset to %v for the new desired state", m.backoff, minBackoff)
	}
}
