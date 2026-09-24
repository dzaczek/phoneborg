package nodeagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dzaczek/phoneborg/proto"
)

// TestPostHeartbeatResponse covers the two shapes a controller may answer
// POST /v1/heartbeat with (ADR-012): 200 with a HeartbeatResponse body, or a
// plain 204 from an older controller that has no desired state to send.
func TestPostHeartbeatResponse(t *testing.T) {
	for _, tc := range []struct {
		name       string
		respond    func(w http.ResponseWriter)
		wantErr    bool
		wantDesire *proto.DesiredRuntime
	}{
		{
			name: "200 with desired model",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"desired":{"model_id":"qwen2.5-0.5b","url":"/v1/model-files/qwen2.5-0.5b","sha256":"abc","size_bytes":492830720,"ctx_size":4096,"slots":1,"kv_type":"auto","layers":24,"kv_heads":2,"head_dim":64}}`))
			},
			wantDesire: &proto.DesiredRuntime{
				ModelID: "qwen2.5-0.5b", URL: "/v1/model-files/qwen2.5-0.5b", SHA256: "abc",
				SizeBytes: 492830720, CtxSize: 4096, Slots: 1, KVType: "auto", Layers: 24, KVHeads: 2, HeadDim: 64,
			},
		},
		{
			name:    "200 with no desired state",
			respond: func(w http.ResponseWriter) { w.Write([]byte(`{}`)) },
		},
		{
			name:    "204 from an older controller",
			respond: func(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tc.respond(w)
			}))
			defer srv.Close()

			a := New(Config{ControllerURL: srv.URL}, nil, discardLog())
			var resp proto.HeartbeatResponse
			if err := a.post(context.Background(), "/v1/heartbeat", proto.Heartbeat{NodeID: "n1"}, &resp); err != nil {
				t.Fatalf("post: %v", err)
			}
			switch {
			case tc.wantDesire == nil && resp.Desired != nil:
				t.Fatalf("Desired = %+v, want nil", resp.Desired)
			case tc.wantDesire != nil:
				if resp.Desired == nil || *resp.Desired != *tc.wantDesire {
					t.Fatalf("Desired = %+v, want %+v", resp.Desired, tc.wantDesire)
				}
			}
		})
	}
}
