package nodeagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseSelfTest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		want    selfTestResult
		wantErr bool
	}{
		{
			name: "ok",
			body: `{"choices":[{"message":{"content":"hi"}}],"timings":{"prompt_per_second":23.5,"predicted_per_second":12.1}}`,
			want: selfTestResult{PromptTPS: 23.5, GenTPS: 12.1},
		},
		{name: "no timings", body: `{"choices":[{"message":{"content":"hi"}}]}`, wantErr: true},
		{name: "invalid json", body: `not json`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSelfTest([]byte(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestSelfTestRequest(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}],"timings":{"prompt_per_second":40.2,"predicted_per_second":9.6}}`))
	}))
	defer srv.Close()

	got, err := selfTestRequest(context.Background(), srv.Client(), srv.URL, "qwen2.5-0.5b")
	if err != nil {
		t.Fatal(err)
	}
	if want := (selfTestResult{PromptTPS: 40.2, GenTPS: 9.6}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if gotBody["model"] != "qwen2.5-0.5b" || gotBody["max_tokens"] != float64(32) ||
		gotBody["ignore_eos"] != true || gotBody["cache_prompt"] != false || gotBody["temperature"] != float64(0) {
		t.Fatalf("request body = %+v", gotBody)
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %+v", gotBody["messages"])
	}
	content, _ := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(content, "phones") {
		t.Fatalf("prompt content = %q", content)
	}
}

func TestSelfTestRequestUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("loading model"))
	}))
	defer srv.Close()

	if _, err := selfTestRequest(context.Background(), srv.Client(), srv.URL, "m"); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}
