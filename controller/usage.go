package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dzaczek/phoneborg/controller/gateway"
)

// UsageCounters accumulate request outcomes for one API key or one node.
type UsageCounters struct {
	Requests           int64 `json:"requests"`
	Errors             int64 `json:"errors"` // outcomes other than HTTP 200
	PromptTokens       int64 `json:"prompt_tokens"`
	CachedPromptTokens int64 `json:"cached_prompt_tokens"`
	CompletionTokens   int64 `json:"completion_tokens"`
	// Completion tokens whose generation speed was reported, and the time
	// spent generating them; AvgGenTPS is their ratio (token-weighted).
	GenTokens  int64     `json:"gen_tokens"`
	GenSeconds float64   `json:"gen_seconds"`
	AvgGenTPS  float64   `json:"avg_gen_tokens_per_second"`
	LastUsed   time.Time `json:"last_used,omitzero"`
}

// UsageTotals is usage since a point in time, by API key name and node id.
type UsageTotals struct {
	Since time.Time                `json:"since"`
	Keys  map[string]UsageCounters `json:"keys"`
	Nodes map[string]UsageCounters `json:"nodes"`
}

func newTotals(since time.Time) UsageTotals {
	return UsageTotals{Since: since, Keys: map[string]UsageCounters{}, Nodes: map[string]UsageCounters{}}
}

func (t UsageTotals) clone() UsageTotals {
	out := newTotals(t.Since)
	for k, v := range t.Keys {
		out.Keys[k] = v.withAvg()
	}
	for k, v := range t.Nodes {
		out.Nodes[k] = v.withAvg()
	}
	return out
}

func (c UsageCounters) withAvg() UsageCounters {
	c.AvgGenTPS = 0
	if c.GenSeconds > 0 {
		c.AvgGenTPS = float64(c.GenTokens) / c.GenSeconds
	}
	return c
}

func (c *UsageCounters) add(e gateway.UsageEvent, now time.Time) {
	c.Requests++
	if e.Code != "200" {
		c.Errors++
	}
	c.PromptTokens += int64(e.PromptTokens)
	c.CachedPromptTokens += int64(e.CachedTokens)
	c.CompletionTokens += int64(e.CompletionTokens)
	if e.GenTPS > 0 && e.CompletionTokens > 0 {
		c.GenTokens += int64(e.CompletionTokens)
		c.GenSeconds += float64(e.CompletionTokens) / e.GenTPS
	}
	c.LastUsed = now
}

// record applies e to the totals: per key once per client request, per node
// once per attempt. A failed attempt that the gateway retries elsewhere is a
// node error but not a client request.
func (t UsageTotals) record(e gateway.UsageEvent, now time.Time) {
	if e.Principal != "" && e.Code != gateway.CodeUpstreamError {
		c := t.Keys[e.Principal]
		c.add(e, now)
		t.Keys[e.Principal] = c
	}
	if e.NodeID != "" {
		c := t.Nodes[e.NodeID]
		c.add(e, now)
		t.Nodes[e.NodeID] = c
	}
}

// Usage aggregates gateway usage since the controller started and, when a
// state directory is set, since first use (persisted in usage.json).
// It implements gateway.UsageRecorder.
type Usage struct {
	path    string // empty = no persistence
	now     func() time.Time
	started time.Time

	mu       sync.Mutex
	session  UsageTotals
	lifetime UsageTotals
	dirty    bool
}

type usageFile struct {
	Version int `json:"version"`
	UsageTotals
}

// NewUsage loads <stateDir>/usage.json if it exists. An empty stateDir keeps
// usage in memory only. A corrupt file is an error rather than being
// silently overwritten.
func NewUsage(stateDir string) (*Usage, error) {
	now := time.Now().UTC()
	u := &Usage{now: time.Now, started: now, session: newTotals(now), lifetime: newTotals(now)}
	if stateDir == "" {
		return u, nil
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	u.path = filepath.Join(stateDir, "usage.json")
	data, err := os.ReadFile(u.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return u, nil
	case err != nil:
		return nil, err
	}
	var f usageFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w (move it away to start from zero)", u.path, err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported version %d", u.path, f.Version)
	}
	u.lifetime = f.UsageTotals.clone()
	return u, nil
}

// Record implements gateway.UsageRecorder.
func (u *Usage) Record(e gateway.UsageEvent) {
	now := u.now().UTC()
	u.mu.Lock()
	defer u.mu.Unlock()
	u.session.record(e, now)
	u.lifetime.record(e, now)
	u.dirty = true
}

// Started is when this controller process started collecting.
func (u *Usage) Started() time.Time { return u.started }

// Session returns usage since the controller started.
func (u *Usage) Session() UsageTotals {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.session.clone()
}

// Lifetime returns usage since first use; ok is false without persistence.
func (u *Usage) Lifetime() (t UsageTotals, ok bool) {
	if u.path == "" {
		return UsageTotals{}, false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.lifetime.clone(), true
}

// Flush writes lifetime usage to disk if it changed. Call periodically and
// on shutdown.
func (u *Usage) Flush() error {
	if u.path == "" {
		return nil
	}
	u.mu.Lock()
	if !u.dirty {
		u.mu.Unlock()
		return nil
	}
	data, err := json.MarshalIndent(usageFile{Version: 1, UsageTotals: u.lifetime.clone()}, "", " ")
	u.dirty = false
	u.mu.Unlock()
	if err == nil {
		err = gateway.WriteFileAtomic(u.path, data, 0o600)
	}
	if err != nil {
		u.mu.Lock()
		u.dirty = true // try again next time
		u.mu.Unlock()
	}
	return err
}
