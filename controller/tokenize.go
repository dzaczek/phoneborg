package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dzaczek/phoneborg/controller/gateway"
)

// Token analyzer (docs/DECISIONS.md ADR-032): counts a text's tokens with
// every model the phones serve, using their own llama-server tokenizers, and
// splits it into tokens for one of them. Tokenizing needs no slot, so busy
// phones answer too.

const (
	tokenizeMaxText   = 256 << 10 // bytes
	tokenizeMaxPieces = 20000
	tokenizeTimeout   = 30 * time.Second
)

// TokenizeRequest is POST /admin/tokenize.
type TokenizeRequest struct {
	Text  string `json:"text"`
	Model string `json:"model,omitempty"` // whose pieces to return; "" = the first model
}

// TokenizeModel is one model's count.
type TokenizeModel struct {
	Model     string  `json:"model"`
	NodeID    string  `json:"node_id"`
	Tokens    int     `json:"tokens"`
	CtxSize   int     `json:"ctx_size,omitempty"`
	PromptTPS float64 `json:"prompt_tps,omitempty"` // the node's self-test prompt speed
	Error     string  `json:"error,omitempty"`
}

// TokenPiece is one token of the split text.
type TokenPiece struct {
	ID    int    `json:"id"`
	Piece string `json:"piece"`
}

// TokenizeResult is the answer of POST /admin/tokenize.
type TokenizeResult struct {
	Chars       int             `json:"chars"`
	Bytes       int             `json:"bytes"`
	Models      []TokenizeModel `json:"models"`
	PiecesModel string          `json:"pieces_model,omitempty"`
	Pieces      []TokenPiece    `json:"pieces,omitempty"`
	Truncated   bool            `json:"truncated,omitempty"` // more tokens than tokenizeMaxPieces
}

var errNoTokenizer = errors.New("no phone serves a model to tokenize with")

// tokenizerBackends returns one phone per served model, sorted by model.
// External nodes are skipped: only llama-server has /tokenize.
func tokenizerBackends(bs []gateway.Backend) []gateway.Backend {
	byModel := map[string]gateway.Backend{}
	for _, b := range bs {
		if strings.HasPrefix(b.NodeID, ExternalPrefix) || b.Model == "" {
			continue
		}
		if cur, ok := byModel[b.Model]; !ok || (cur.Hot && !b.Hot) {
			byModel[b.Model] = b
		}
	}
	out := make([]gateway.Backend, 0, len(byModel))
	for _, b := range byModel {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// llamaTokenize calls llama-server's /tokenize.
func llamaTokenize(ctx context.Context, client *http.Client, baseURL, text string, pieces bool) ([]TokenPiece, error) {
	body, _ := json.Marshal(map[string]any{"content": text, "add_special": false, "with_pieces": pieces})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/tokenize", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tokenize: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Tokens []json.RawMessage `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("tokenize: %w", err)
	}
	toks := make([]TokenPiece, len(out.Tokens))
	for i, t := range out.Tokens {
		if !pieces {
			_ = json.Unmarshal(t, &toks[i].ID)
			continue
		}
		var p struct {
			ID    int             `json:"id"`
			Piece json.RawMessage `json:"piece"`
		}
		if err := json.Unmarshal(t, &p); err != nil {
			return nil, fmt.Errorf("tokenize: %w", err)
		}
		toks[i] = TokenPiece{ID: p.ID, Piece: pieceText(p.Piece)}
	}
	return toks, nil
}

// pieceText decodes a piece: a string, or a byte array when the token is
// not valid UTF-8 on its own (part of a multi-byte character).
func pieceText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var b []int
	if json.Unmarshal(raw, &b) != nil {
		return ""
	}
	var sb strings.Builder
	for _, x := range b {
		fmt.Fprintf(&sb, "<0x%02X>", x)
	}
	return sb.String()
}

func (s *Server) tokenize(ctx context.Context, req TokenizeRequest) (TokenizeResult, error) {
	res := TokenizeResult{Chars: utf8.RuneCountInString(req.Text), Bytes: len(req.Text)}
	bs := tokenizerBackends(s.gw.Backends())
	if len(bs) == 0 {
		return res, errNoTokenizer
	}
	piecesModel := req.Model
	if piecesModel == "" {
		piecesModel = bs[0].Model
	}
	promptTPS := map[string]float64{}
	nodes, _ := s.reg.View()
	for _, n := range nodes {
		if hb := n.LastHeartbeat; hb != nil && hb.Runtime != nil {
			promptTPS[n.ID] = hb.Runtime.PromptTPS
		}
	}
	client := &http.Client{Timeout: tokenizeTimeout}
	res.Models = make([]TokenizeModel, len(bs))
	var wg sync.WaitGroup
	for i, b := range bs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := TokenizeModel{Model: b.Model, NodeID: b.NodeID, CtxSize: b.CtxSize, PromptTPS: promptTPS[b.NodeID]}
			toks, err := llamaTokenize(ctx, client, b.URL, req.Text, b.Model == piecesModel)
			if err != nil {
				m.Error = err.Error()
			} else {
				m.Tokens = len(toks)
				if b.Model == piecesModel {
					res.PiecesModel = b.Model
					if len(toks) > tokenizeMaxPieces {
						toks, res.Truncated = toks[:tokenizeMaxPieces], true
					}
					res.Pieces = toks
				}
			}
			res.Models[i] = m
		}()
	}
	wg.Wait()
	return res, nil
}

func (s *Server) registerTokenizeAdmin(add func(pattern, action string, fn http.HandlerFunc)) {
	add("POST /admin/tokenize", "tokenize", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, tokenizeMaxText+4096)
		var req TokenizeRequest
		if !decodeStrict(w, r, &req) {
			return
		}
		if req.Text == "" || len(req.Text) > tokenizeMaxText {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("text must be 1 to %d bytes", tokenizeMaxText))
			return
		}
		res, err := s.tokenize(r.Context(), req)
		if errors.Is(err, errNoTokenizer) {
			httpError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
}
