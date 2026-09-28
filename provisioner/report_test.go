package provisioner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dzaczek/phoneborg/proto"
)

func TestReporterReportRoundTrip(t *testing.T) {
	var gotReq proto.DeviceReport
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != proto.PathDevicesReport || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(proto.DeviceReportResponse{
			AutoProvision: false,
			Commands:      []proto.DeviceCommand{{Serial: "S1"}},
		})
	}))
	defer srv.Close()

	r := NewReporter(srv.URL, "test-token")
	devs := []proto.DeviceStatus{{Serial: "S1", ADBState: "device", Status: proto.DeviceNew}}
	resp, err := r.Report(context.Background(), devs, []string{"S0"})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if resp.AutoProvision || len(resp.Commands) != 1 || resp.Commands[0].Serial != "S1" {
		t.Fatalf("response = %+v", resp)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotReq.Host != r.Host || len(gotReq.Devices) != 1 || gotReq.Devices[0].Serial != "S1" || len(gotReq.Acked) != 1 || gotReq.Acked[0] != "S0" {
		t.Fatalf("request = %+v", gotReq)
	}
}

func TestReporterReportHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid or missing admin token"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	r := NewReporter(srv.URL, "wrong")
	if _, err := r.Report(context.Background(), nil, nil); err == nil {
		t.Fatal("expected an error on HTTP 401")
	}
}
