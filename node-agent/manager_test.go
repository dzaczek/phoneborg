package nodeagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
