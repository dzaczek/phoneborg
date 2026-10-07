package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/dzaczek/phoneborg/controller/gateway"
	"github.com/dzaczek/phoneborg/proto"
)

// fakeTokenizer answers /tokenize like llama-server: one token per word, a
// byte-array piece for the word BYTES.
func fakeTokenizer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tokenize" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Content    string `json:"content"`
			WithPieces bool   `json:"with_pieces"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var toks []any
		for i, w := range strings.Fields(req.Content) {
			switch {
			case !req.WithPieces:
				toks = append(toks, i)
			case w == "BYTES":
				toks = append(toks, map[string]any{"id": i, "piece": []int{255}})
			default:
				toks = append(toks, map[string]any{"id": i, "piece": w})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"tokens": toks})
	}))
}

func TestTokenizerBackends(t *testing.T) {
	got := tokenizerBackends([]gateway.Backend{
		{NodeID: "a", Model: "qwen", Hot: true}, {NodeID: "b", Model: "qwen"}, {NodeID: "c", Model: "gemma"},
		{NodeID: ExternalPrefix + "mac", Model: "big"}, {NodeID: "d"},
	})
	if len(got) != 2 || got[0].Model != "gemma" || got[1].NodeID != "b" {
		t.Errorf("got %+v: want one cool phone per model, no external nodes", got)
	}
}

func TestPieceText(t *testing.T) {
	if pieceText(json.RawMessage(`" ja"`)) != " ja" || pieceText(json.RawMessage(`[240,159]`)) != "<0xF0><0x9F>" {
		t.Error("pieceText")
	}
}

func TestTokenizeAdmin(t *testing.T) {
	e := newEnv(t, testToken, nil)
	e.admin(http.MethodPost, "/admin/tokenize", `{"text":"hi"}`, http.StatusServiceUnavailable, nil) // no phone yet
	for _, n := range []struct{ id, model string }{{"p1", "qwen"}, {"p2", "gemma"}} {
		u, _ := url.Parse(fakeTokenizer(t).URL)
		port, _ := strconv.Atoi(u.Port())
		e.reg.Register(proto.RegisterRequest{NodeID: n.id}, "x")
		_ = e.reg.ReportBenchmark(proto.BenchmarkReport{NodeID: n.id})
		_ = e.reg.Heartbeat(proto.Heartbeat{NodeID: n.id, Runtime: &proto.RuntimeStatus{Model: n.model, Ready: true,
			AdvertiseHost: u.Hostname(), AdvertisePort: port, CtxSize: 16384, PromptTPS: 50}})
	}
	var res TokenizeResult
	e.admin(http.MethodPost, "/admin/tokenize", `{"text":"one two BYTES four","model":"qwen"}`, http.StatusOK, &res)
	if len(res.Models) != 2 || res.Models[0].Model != "gemma" || res.Models[0].Tokens != 4 || res.Models[1].Tokens != 4 ||
		res.Models[1].CtxSize != 16384 || res.Models[1].PromptTPS != 50 {
		t.Fatalf("models = %+v", res.Models)
	}
	if res.PiecesModel != "qwen" || len(res.Pieces) != 4 || res.Pieces[2].Piece != "<0xFF>" || res.Chars != 18 {
		t.Errorf("pieces = %q %+v, chars %d", res.PiecesModel, res.Pieces, res.Chars)
	}
	e.admin(http.MethodPost, "/admin/tokenize", `{"text":""}`, http.StatusBadRequest, nil)
	e.admin(http.MethodPost, "/admin/tokenize", `{"text":"`+strings.Repeat("a", tokenizeMaxText+1)+`"}`, http.StatusBadRequest, nil)
}
