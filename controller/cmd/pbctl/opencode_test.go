package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- pure helpers ----

func TestHasJSONComments(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want bool
	}{
		{"plain", `{"a": 1, "b": "x"}`, false},
		{"line comment", "{\n  // hi\n  \"a\": 1\n}", true},
		{"block comment", "{ /* hi */ \"a\": 1 }", true},
		{"slash in string", `{"a": "http://x/y"}`, false},
		{"escaped quote then comment", `{"a": "x\"y" /* c */}`, true},
		{"empty", ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasJSONComments([]byte(tc.data)); got != tc.want {
				t.Errorf("hasJSONComments(%q) = %v, want %v", tc.data, got, tc.want)
			}
		})
	}
}

func TestIsJSONCConfig(t *testing.T) {
	if !isJSONCConfig("x/opencode.jsonc", []byte(`{}`)) {
		t.Error(".jsonc extension should always be JSONC")
	}
	if isJSONCConfig("x/opencode.json", []byte(`{"a":1}`)) {
		t.Error("plain .json without comments should not be JSONC")
	}
	if !isJSONCConfig("x/opencode.json", []byte("{\n// c\n}")) {
		t.Error(".json with a comment should be detected as JSONC")
	}
}

func TestPlainJSONHasProviderKey(t *testing.T) {
	has, err := plainJSONHasProviderKey([]byte(`{"provider":{"phoneborg":{}}}`), "phoneborg")
	if err != nil || !has {
		t.Fatalf("has=%v err=%v", has, err)
	}
	has, err = plainJSONHasProviderKey([]byte(`{"provider":{"other":{}}}`), "phoneborg")
	if err != nil || has {
		t.Fatalf("has=%v err=%v", has, err)
	}
	has, err = plainJSONHasProviderKey([]byte(`{"theme":"dark"}`), "phoneborg")
	if err != nil || has {
		t.Fatalf("has=%v err=%v", has, err)
	}
}

func TestMergeProviderBlockJSONPreservesOtherKeys(t *testing.T) {
	orig := `{"theme":"dark","provider":{"other":{"npm":"x"}}}`
	block := ocProviderBlock{NPM: "@ai-sdk/openai-compatible", Name: "PhoneBorg",
		Options: ocProviderOptions{BaseURL: "http://h/v1", APIKey: "{env:PHONEBORG_API_KEY}", HeaderTimeout: 1800000, ChunkTimeout: 1800000},
		Models:  map[string]ocProviderModel{"auto": {Name: "Auto", Limit: ocLimit{Context: 16384, Output: 2048}}}}
	merged, err := mergeProviderBlockJSON([]byte(orig), "phoneborg", block)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(merged, &root); err != nil {
		t.Fatal(err)
	}
	if root["theme"] != "dark" {
		t.Errorf("theme key lost: %v", root)
	}
	providers, _ := root["provider"].(map[string]any)
	if providers["other"] == nil {
		t.Errorf("other provider lost: %v", providers)
	}
	pb, _ := providers["phoneborg"].(map[string]any)
	if pb["npm"] != "@ai-sdk/openai-compatible" {
		t.Errorf("phoneborg block not merged: %v", providers)
	}
}

func TestRenderAgentGolden(t *testing.T) {
	spec := ocAgentSpec{Name: "borg-fast", Description: "Fast tool-less worker on the phone pool (pool/fast).",
		Model: "phoneborg/pool/fast", ReadTools: false, Body: bodyBorgFast}
	got := renderAgent(spec)
	want := "---\n" +
		"description: Fast tool-less worker on the phone pool (pool/fast).\n" +
		"mode: subagent\n" +
		"model: phoneborg/pool/fast\n" +
		"tools:\n" +
		"  \"*\": false\n" +
		"---\n" +
		bodyBorgFast + "\n\n" +
		ocGeneratedMarker + "\n"
	if got != want {
		t.Fatalf("renderAgent mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if lines[len(lines)-1] != ocGeneratedMarker {
		t.Errorf("last line = %q, want marker", lines[len(lines)-1])
	}
}

func TestRenderAgentReadTools(t *testing.T) {
	spec := ocAgentSpec{Name: "borg-fast", Description: "d", Model: "phoneborg/pool/fast", ReadTools: true, Body: "hi"}
	got := renderAgent(spec)
	for _, want := range []string{"\"*\": false\n", "read: true\n", "grep: true\n", "glob: true\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestParseAgentFrontmatterRoundTrip(t *testing.T) {
	specs := []ocAgentSpec{
		borgFastAgentSpec("phoneborg", false),
		borgSummarizeAgentSpec("phoneborg", true),
		phoneAgentSpec("phoneborg", aliasedNode{Alias: "phone-01", Device: "Xiaomi Mi 8", ServedModel: "qwen2.5-0.5b"}, false),
	}
	for _, spec := range specs {
		content := renderAgent(spec)
		model, body, err := parseAgentFrontmatter(content)
		if err != nil {
			t.Fatalf("%s: %v", spec.Name, err)
		}
		if model != spec.Model {
			t.Errorf("%s: model = %q, want %q", spec.Name, model, spec.Model)
		}
		if body != strings.TrimRight(spec.Body, "\n") {
			t.Errorf("%s: body = %q, want %q", spec.Name, body, spec.Body)
		}
	}
}

func TestParseAgentFrontmatterErrors(t *testing.T) {
	for _, bad := range []string{"", "no frontmatter here", "---\nmodel: x\nno closing fence"} {
		if _, _, err := parseAgentFrontmatter(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestAliasedNodesFromModels(t *testing.T) {
	nodes := []ocAdminNode{
		{ID: "abc123", Alias: "phone-01", Inventory: struct {
			Manufacturer string `json:"manufacturer"`
			Model        string `json:"model"`
		}{Manufacturer: "Xiaomi", Model: "Mi 8"}},
	}
	models := []ocModel{
		{ID: "auto", Kind: "auto"},
		{ID: "pool/fast", Kind: "pool", Nodes: 2},
		{ID: "node/phone-01", Kind: "node", Nodes: 1, Model: "qwen2.5-0.5b", Ready: true},
		{ID: "node/phone-02", Kind: "node", Nodes: 0, Ready: false}, // no matching admin node
	}
	got := aliasedNodesFromModels(models, nodes)
	if len(got) != 2 {
		t.Fatalf("got %d aliased nodes, want 2: %+v", len(got), got)
	}
	if got[0].Alias != "phone-01" || got[0].Device != "Xiaomi Mi 8" || got[0].ServedModel != "qwen2.5-0.5b" || !got[0].Ready {
		t.Errorf("phone-01: %+v", got[0])
	}
	if got[1].Alias != "phone-02" || got[1].Device != "" || got[1].NodesReady != 0 {
		t.Errorf("phone-02: %+v", got[1])
	}
}

func adminNode(id, alias, model string, ctx int) ocAdminNode {
	n := ocAdminNode{ID: id, Alias: alias}
	n.LastHeartbeat = &struct {
		Runtime *struct {
			Model   string `json:"model"`
			Ready   bool   `json:"ready"`
			CtxSize int    `json:"ctx_size"`
		} `json:"runtime"`
	}{}
	n.LastHeartbeat.Runtime = &struct {
		Model   string `json:"model"`
		Ready   bool   `json:"ready"`
		CtxSize int    `json:"ctx_size"`
	}{Model: model, Ready: true, CtxSize: ctx}
	return n
}

func TestModelContextLimit(t *testing.T) {
	nodes := []ocAdminNode{
		adminNode("n1", "phone-01", "qwen2.5-0.5b", 32768),
		adminNode("n2", "", "qwen2.5-0.5b", 8192),
	}
	if got := modelContextLimit(ocModel{ID: "node/phone-01", Kind: "node"}, nodes); got != 32768 {
		t.Errorf("node ctx = %d, want 32768", got)
	}
	if got := modelContextLimit(ocModel{ID: "qwen2.5-0.5b", Kind: "model"}, nodes); got != 32768 {
		t.Errorf("model ctx (max across nodes) = %d, want 32768", got)
	}
	if got := modelContextLimit(ocModel{ID: "auto", Kind: "auto"}, nodes); got != ocDefaultCtx {
		t.Errorf("auto ctx = %d, want default %d", got, ocDefaultCtx)
	}
	if got := modelContextLimit(ocModel{ID: "node/phone-99", Kind: "node"}, nodes); got != ocDefaultCtx {
		t.Errorf("unknown node ctx = %d, want default %d", got, ocDefaultCtx)
	}
}

func TestModelDisplayName(t *testing.T) {
	nodes := []ocAdminNode{adminNode("n1", "phone-01", "qwen2.5-0.5b", 16384)}
	if got := modelDisplayName(ocModel{ID: "auto", Kind: "auto"}, nodes); got != "Auto (any ready phone)" {
		t.Errorf("auto: %q", got)
	}
	if got := modelDisplayName(ocModel{ID: "pool/fast", Kind: "pool", Description: "Fast pool"}, nodes); got != "Fast pool" {
		t.Errorf("pool with description: %q", got)
	}
	if got := modelDisplayName(ocModel{ID: "pool/fast", Kind: "pool"}, nodes); got != "Pool fast" {
		t.Errorf("pool without description: %q", got)
	}
	if got := modelDisplayName(ocModel{ID: "node/phone-01", Kind: "node"}, nodes); got != "Phone phone-01 (qwen2.5-0.5b)" {
		t.Errorf("node: %q", got)
	}
	if got := modelDisplayName(ocModel{ID: "qwen2.5-0.5b", Kind: "model"}, nodes); got != "qwen2.5-0.5b" {
		t.Errorf("real model: %q", got)
	}
}

func TestBuildProviderBlock(t *testing.T) {
	nodes := []ocAdminNode{adminNode("n1", "phone-01", "qwen2.5-0.5b", 32768)}
	models := []ocModel{
		{ID: "qwen2.5-0.5b", Kind: "model"},
		{ID: "auto", Kind: "auto"},
		{ID: "pool/fast", Kind: "pool", Description: "Fast pool"},
		{ID: "node/phone-01", Kind: "node", Model: "qwen2.5-0.5b"},
	}
	pb := buildProviderBlock("http://127.0.0.1:18080/v1", models, nodes)
	if pb.NPM != "@ai-sdk/openai-compatible" || pb.Name != "PhoneBorg" {
		t.Errorf("npm/name: %+v", pb)
	}
	if pb.Options.BaseURL != "http://127.0.0.1:18080/v1" || pb.Options.APIKey != "{env:PHONEBORG_API_KEY}" {
		t.Errorf("options: %+v", pb.Options)
	}
	if pb.Options.Timeout != false || pb.Options.HeaderTimeout != 1800000 || pb.Options.ChunkTimeout != 1800000 {
		t.Errorf("timeouts: %+v", pb.Options)
	}
	if len(pb.Models) != 4 {
		t.Fatalf("models = %d, want 4: %+v", len(pb.Models), pb.Models)
	}
	if pb.Models["node/phone-01"].Limit.Context != 32768 {
		t.Errorf("node ctx = %d", pb.Models["node/phone-01"].Limit.Context)
	}
	if pb.Models["auto"].Limit.Context != ocDefaultCtx || pb.Models["auto"].Limit.Output != ocOutputLimit {
		t.Errorf("auto limit: %+v", pb.Models["auto"].Limit)
	}
}

func TestTargetForAgentName(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"borg-fast", "pool/fast"}, {"borg-summarize", "pool/fast"}, {"borg-review", "pool/fast"},
		{"phone-phone-01", "node/phone-01"}, {"phone-01", "node/01"},
	} {
		if got := targetForAgentName(tc.name); got != tc.want {
			t.Errorf("targetForAgentName(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSummarizePrewarmResults(t *testing.T) {
	ok, ms, errs := summarizePrewarmResults(nil)
	if ok != "0/0" || ms != "-" || errs != "" {
		t.Errorf("empty: %q %q %q", ok, ms, errs)
	}
	ok, ms, errs = summarizePrewarmResults([]ocPrewarmNodeResult{
		{OK: true, MS: 12}, {OK: false, MS: 30, Error: "timeout"},
	})
	if ok != "1/2" || ms != "30" || errs != "timeout" {
		t.Errorf("mixed: %q %q %q", ok, ms, errs)
	}
}

func TestManifestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := ocManifest{Version: 1, Provider: "phoneborg", BaseURL: "http://h/v1", ReadTools: true,
		ConfigPath: "opencode.json", ConfigManaged: true, Files: map[string]string{"opencode.json": "abc", ocAgentDirName + "/borg-fast.md": "def"}}
	if err := m.save(dir); err != nil {
		t.Fatal(err)
	}
	got, ok, err := loadManifest(dir)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.Provider != "phoneborg" || !got.ReadTools || !got.ConfigManaged || got.Files["opencode.json"] != "abc" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
	names := got.agentNames()
	if len(names) != 1 || names[0] != "borg-fast" {
		t.Errorf("agentNames = %v", names)
	}

	if _, ok, err := loadManifest(t.TempDir()); ok || err != nil {
		t.Errorf("missing manifest: ok=%v err=%v", ok, err)
	}
}

func TestSyncAgentFilesAddUpdateRemoveKeep(t *testing.T) {
	dir := t.TempDir()
	manifest := ocManifest{Files: map[string]string{}}

	base := []aliasedNode{{Alias: "phone-01", Device: "Xiaomi Mi 8", ServedModel: "qwen2.5-0.5b"}}
	specs := managedAgentSpecs("phoneborg", false, base)
	res, err := syncAgentFiles(dir, specs, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 4 || len(res.Updated) != 0 || len(res.Removed) != 0 || len(res.Kept) != 0 {
		t.Fatalf("first sync: %+v", res)
	}

	// Idempotent: same specs again -> everything kept.
	res, err = syncAgentFiles(dir, specs, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Kept) != 4 || res.changed() {
		t.Fatalf("second sync should be all-kept: %+v", res)
	}

	// Template change (read-tools toggled) -> existing files updated.
	specs2 := managedAgentSpecs("phoneborg", true, base)
	res, err = syncAgentFiles(dir, specs2, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Updated) != 4 || len(res.Added) != 0 || len(res.Removed) != 0 {
		t.Fatalf("template update: %+v", res)
	}

	// Alias removed -> phone-01 file deleted; new alias phone-02 added.
	next := []aliasedNode{{Alias: "phone-02", Device: "Pixel 5", ServedModel: "qwen2.5-0.5b"}}
	specs3 := managedAgentSpecs("phoneborg", true, next)
	res, err = syncAgentFiles(dir, specs3, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 1 || res.Added[0] != ocAgentDirName+"/phone-phone-02.md" {
		t.Errorf("added: %+v", res.Added)
	}
	if len(res.Removed) != 1 || res.Removed[0] != ocAgentDirName+"/phone-phone-01.md" {
		t.Errorf("removed: %+v", res.Removed)
	}
	if fileExists(filepath.Join(dir, ocAgentDirName, "phone-phone-01.md")) {
		t.Error("phone-phone-01.md should have been deleted")
	}
}

func TestSyncAgentFilesKeepsHandEditedFile(t *testing.T) {
	dir := t.TempDir()
	manifest := ocManifest{Files: map[string]string{}}
	aliased := []aliasedNode{{Alias: "phone-01", Device: "Xiaomi Mi 8", ServedModel: "qwen2.5-0.5b"}}
	specs := managedAgentSpecs("phoneborg", false, aliased)
	if _, err := syncAgentFiles(dir, specs, &manifest); err != nil {
		t.Fatal(err)
	}

	fastPath := filepath.Join(dir, ocAgentDirName, "borg-fast.md")
	if err := os.WriteFile(fastPath, []byte("hand edited content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Still wanted, but edited: must be kept as-is, with a warning.
	res, err := syncAgentFiles(dir, specs, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "edited by hand") {
		t.Errorf("expected hand-edit warning: %+v", res)
	}
	data, _ := os.ReadFile(fastPath)
	if string(data) != "hand edited content\n" {
		t.Error("hand-edited file was overwritten")
	}

	// No longer wanted (alias removed) and edited: must be kept, not deleted.
	res, err = syncAgentFiles(dir, nil, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !fileExists(fastPath) {
		t.Error("hand-edited file should not have been removed")
	}
	foundWarn := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "not removing") {
			foundWarn = true
		}
	}
	if !foundWarn {
		t.Errorf("expected 'not removing' warning: %+v", res.Warnings)
	}
}

// ---- fake cluster server for command-level tests ----

type fakeState struct {
	mu          sync.Mutex
	token       string
	pools       []ocPool
	poolsErr    int
	nodes       []ocAdminNode
	models      []ocModel
	prewarm     []ocPrewarmNodeResult
	prewarmReqs []ocPrewarmRequest
}

func newFakeServer(t *testing.T, st *fakeState) *httptest.Server {
	t.Helper()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+st.token {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return false
		}
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/pools", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		if !auth(w, r) {
			return
		}
		if st.poolsErr != 0 {
			w.WriteHeader(st.poolsErr)
			json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
			return
		}
		json.NewEncoder(w).Encode(ocPoolList{Pools: st.pools})
	})
	mux.HandleFunc("PUT /admin/pools/", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		if !auth(w, r) {
			return
		}
		if st.poolsErr != 0 {
			w.WriteHeader(st.poolsErr)
			json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
			return
		}
		var p ocPool
		json.NewDecoder(r.Body).Decode(&p)
		st.pools = append(st.pools, p)
		json.NewEncoder(w).Encode(p)
	})
	mux.HandleFunc("GET /admin/nodes", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		if !auth(w, r) {
			return
		}
		json.NewEncoder(w).Encode(st.nodes)
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		json.NewEncoder(w).Encode(struct {
			Object string    `json:"object"`
			Data   []ocModel `json:"data"`
		}{Object: "list", Data: st.models})
	})
	mux.HandleFunc("POST /admin/prewarm", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		if !auth(w, r) {
			return
		}
		var req ocPrewarmRequest
		json.NewDecoder(r.Body).Decode(&req)
		st.prewarmReqs = append(st.prewarmReqs, req)
		json.NewEncoder(w).Encode(ocPrewarmResponse{Results: st.prewarm})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func fakeClient(url string) *client {
	return &client{base: url, token: "tok", http: &http.Client{Timeout: 5 * time.Second}}
}

func baseFakeState() *fakeState {
	return &fakeState{
		token: "tok",
		nodes: []ocAdminNode{adminNode("abc123", "phone-01", "qwen2.5-0.5b", 16384)},
		models: []ocModel{
			{ID: "qwen2.5-0.5b", Kind: "model", Nodes: 1},
			{ID: "auto", Kind: "auto", Nodes: 1},
			{ID: "pool/fast", Kind: "pool", Nodes: 1, Description: "Fast pool"},
			{ID: "node/phone-01", Kind: "node", Nodes: 1, Model: "qwen2.5-0.5b", Ready: true},
		},
	}
}

func TestOpencodeInitCreatesConfigAndAgents(t *testing.T) {
	st := baseFakeState()
	ts := newFakeServer(t, st)
	c := fakeClient(ts.URL)
	dir := t.TempDir()
	o := &out{w: new(strings.Builder)}

	if err := opencodeInit(c, o, []string{"-dir", dir}); err != nil {
		t.Fatal(err)
	}

	cfgData, err := os.ReadFile(filepath.Join(dir, "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wrapper ocProviderWrapper
	if err := json.Unmarshal(cfgData, &wrapper); err != nil {
		t.Fatal(err)
	}
	pb, ok := wrapper.Provider["phoneborg"]
	if !ok {
		t.Fatal("missing provider.phoneborg")
	}
	if pb.Options.BaseURL != ts.URL+"/v1" {
		t.Errorf("baseURL = %q", pb.Options.BaseURL)
	}
	for _, id := range []string{"qwen2.5-0.5b", "auto", "pool/fast", "node/phone-01"} {
		if _, ok := pb.Models[id]; !ok {
			t.Errorf("missing model %q in provider block", id)
		}
	}

	for _, name := range []string{"borg-fast", "borg-summarize", "borg-review", "phone-phone-01"} {
		p := filepath.Join(dir, ocAgentDirName, name+".md")
		data, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		if lines[len(lines)-1] != ocGeneratedMarker {
			t.Errorf("%s: last line is not the generated marker", name)
		}
	}

	manifest, ok, err := loadManifest(dir)
	if err != nil || !ok {
		t.Fatalf("manifest: ok=%v err=%v", ok, err)
	}
	if !manifest.ConfigManaged || manifest.Provider != "phoneborg" {
		t.Errorf("manifest: %+v", manifest)
	}
	if len(manifest.agentNames()) != 4 {
		t.Errorf("agentNames = %v", manifest.agentNames())
	}

	st.mu.Lock()
	poolCreated := len(st.pools) == 1 && st.pools[0].Name == "fast" && st.pools[0].Routing == "spread"
	st.mu.Unlock()
	if !poolCreated {
		t.Errorf("fast pool not created: %+v", st.pools)
	}
}

func TestOpencodeInitPoolsNotFoundWarnsAndContinues(t *testing.T) {
	st := baseFakeState()
	st.poolsErr = http.StatusNotFound
	ts := newFakeServer(t, st)
	c := fakeClient(ts.URL)
	dir := t.TempDir()
	var sb strings.Builder
	o := &out{w: &sb}

	if err := opencodeInit(c, o, []string{"-dir", dir}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "pools API") {
		t.Errorf("expected a pools-API warning, got:\n%s", sb.String())
	}
	if !fileExists(filepath.Join(dir, "opencode.json")) {
		t.Error("config should still be written when pools API is missing")
	}
}

func TestOpencodeInitMergesIntoExistingPlainJSON(t *testing.T) {
	st := baseFakeState()
	ts := newFakeServer(t, st)
	c := fakeClient(ts.URL)
	dir := t.TempDir()
	orig := `{"theme":"dark","provider":{"other":{"npm":"x"}}}`
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	o := &out{w: new(strings.Builder)}
	if err := opencodeInit(c, o, []string{"-dir", dir}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	json.Unmarshal(data, &root)
	if root["theme"] != "dark" {
		t.Error("theme key lost on merge")
	}
	providers := root["provider"].(map[string]any)
	if providers["other"] == nil {
		t.Error("existing provider lost on merge")
	}
	if providers["phoneborg"] == nil {
		t.Error("phoneborg provider not merged in")
	}

	bak, err := os.ReadFile(filepath.Join(dir, "opencode.json.bak"))
	if err != nil || string(bak) != orig {
		t.Errorf("backup: err=%v content=%q", err, bak)
	}

	manifest, _, _ := loadManifest(dir)
	if manifest.ConfigManaged {
		t.Error("merging into a pre-existing file must not count as managed (created by init)")
	}
}

func TestOpencodeInitJSONCNeverModified(t *testing.T) {
	st := baseFakeState()
	ts := newFakeServer(t, st)
	c := fakeClient(ts.URL)
	dir := t.TempDir()
	orig := "{\n  // a comment\n  \"theme\": \"dark\"\n}"
	if err := os.WriteFile(filepath.Join(dir, "opencode.jsonc"), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	o := &out{w: &sb}
	if err := opencodeInit(c, o, []string{"-dir", dir}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "opencode.jsonc"))
	if err != nil || string(data) != orig {
		t.Errorf("jsonc file was modified: err=%v", err)
	}
	if !strings.Contains(sb.String(), "phoneborg") {
		t.Errorf("expected the provider block printed to paste, got:\n%s", sb.String())
	}
}

func TestOpencodeInitAlreadyHasProviderNeedsForce(t *testing.T) {
	st := baseFakeState()
	ts := newFakeServer(t, st)
	c := fakeClient(ts.URL)
	dir := t.TempDir()
	orig := `{"provider":{"phoneborg":{"npm":"old"}}}`
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := opencodeInit(c, &out{w: new(strings.Builder)}, []string{"-dir", dir}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "opencode.json"))
	if string(data) != orig {
		t.Error("config should be untouched without -force")
	}

	if err := opencodeInit(c, &out{w: new(strings.Builder)}, []string{"-dir", dir, "-force"}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "opencode.json"))
	var root map[string]any
	json.Unmarshal(data, &root)
	pb := root["provider"].(map[string]any)["phoneborg"].(map[string]any)
	if pb["npm"] != "@ai-sdk/openai-compatible" {
		t.Errorf("-force did not replace the provider block: %v", pb)
	}
}

func TestOpencodeSyncAddsAndRemovesPhoneAgents(t *testing.T) {
	st := baseFakeState()
	ts := newFakeServer(t, st)
	c := fakeClient(ts.URL)
	dir := t.TempDir()
	if err := opencodeInit(c, &out{w: new(strings.Builder)}, []string{"-dir", dir}); err != nil {
		t.Fatal(err)
	}

	// Alias renamed: phone-01 gone, phone-02 appears.
	st.mu.Lock()
	st.nodes = []ocAdminNode{adminNode("abc123", "phone-02", "qwen2.5-0.5b", 16384)}
	for i := range st.models {
		if st.models[i].ID == "node/phone-01" {
			st.models[i].ID = "node/phone-02"
		}
	}
	st.mu.Unlock()

	var sb strings.Builder
	if err := opencodeSync(c, &out{w: &sb}, []string{"-dir", dir}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "added 1, removed 1") {
		t.Errorf("sync summary: %s", sb.String())
	}
	if fileExists(filepath.Join(dir, ocAgentDirName, "phone-phone-01.md")) {
		t.Error("phone-phone-01.md should be removed")
	}
	if !fileExists(filepath.Join(dir, ocAgentDirName, "phone-phone-02.md")) {
		t.Error("phone-phone-02.md should be added")
	}

	// Idempotent: running again with no cluster change yields no changes.
	sb.Reset()
	if err := opencodeSync(c, &out{w: &sb}, []string{"-dir", dir}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "added 0, removed 0, updated 0") {
		t.Errorf("expected idempotent no-op sync, got: %s", sb.String())
	}
}

func TestOpencodeSyncWithoutInitFails(t *testing.T) {
	st := baseFakeState()
	ts := newFakeServer(t, st)
	c := fakeClient(ts.URL)
	dir := t.TempDir()
	err := opencodeSync(c, &out{w: new(strings.Builder)}, []string{"-dir", dir})
	if err == nil || !strings.Contains(err.Error(), "opencode init") {
		t.Fatalf("err = %v", err)
	}
}

func TestOpencodeStatus(t *testing.T) {
	st := baseFakeState()
	ts := newFakeServer(t, st)
	c := fakeClient(ts.URL)
	dir := t.TempDir()
	if err := opencodeInit(c, &out{w: new(strings.Builder)}, []string{"-dir", dir}); err != nil {
		t.Fatal(err)
	}

	var sb strings.Builder
	if err := opencodeStatus(c, &out{w: &sb}, []string{"-dir", dir}); err != nil {
		t.Fatal(err)
	}
	got := sb.String()
	for _, want := range []string{"borg-fast", "pool/fast", "phone-phone-01", "node/phone-01", "ready", "provider phoneborg", ts.URL + "/v1"} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q:\n%s", want, got)
		}
	}
}

func TestOpencodePrewarm(t *testing.T) {
	st := baseFakeState()
	st.prewarm = []ocPrewarmNodeResult{{NodeID: "abc123", Alias: "phone-01", OK: true, MS: 42}}
	ts := newFakeServer(t, st)
	c := fakeClient(ts.URL)
	dir := t.TempDir()
	if err := opencodeInit(c, &out{w: new(strings.Builder)}, []string{"-dir", dir}); err != nil {
		t.Fatal(err)
	}

	var sb strings.Builder
	if err := opencodePrewarm(c, &out{w: &sb}, []string{"-dir", dir}); err != nil {
		t.Fatal(err)
	}
	got := sb.String()
	if !strings.Contains(got, "borg-fast") || !strings.Contains(got, "1/1") || !strings.Contains(got, "42") {
		t.Errorf("prewarm output: %s", got)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.prewarmReqs) != 4 {
		t.Fatalf("expected 4 prewarm requests, got %d", len(st.prewarmReqs))
	}
	for _, req := range st.prewarmReqs {
		if strings.Contains(req.Target, "phoneborg/") {
			t.Errorf("target should have the provider prefix stripped: %q", req.Target)
		}
		if len(req.Messages) != 1 || req.Messages[0].Role != "system" || req.Messages[0].Content == "" {
			t.Errorf("bad prewarm messages: %+v", req.Messages)
		}
	}
}

func TestOpencodeWatchBadInterval(t *testing.T) {
	c := fakeClient("http://127.0.0.1:0")
	err := opencodeWatch(c, &out{w: new(strings.Builder)}, []string{"-interval", "0s"})
	if err == nil || !strings.Contains(err.Error(), "-interval") {
		t.Fatalf("err = %v", err)
	}
}

func TestOpencodeCmdUnknownSubcommand(t *testing.T) {
	c := fakeClient("http://127.0.0.1:0")
	if err := opencodeCmd(c, &out{w: new(strings.Builder)}, []string{"bogus"}); err != errUsage {
		t.Fatalf("err = %v, want errUsage", err)
	}
	if err := opencodeCmd(c, &out{w: new(strings.Builder)}, nil); err != errUsage {
		t.Fatalf("err = %v, want errUsage", err)
	}
}

// TestOpencodeThroughPbctl exercises init through the top-level dispatcher,
// the same path a real "pbctl opencode init" invocation takes.
func TestOpencodeThroughPbctl(t *testing.T) {
	st := baseFakeState()
	ts := newFakeServer(t, st)
	dir := t.TempDir()
	env := map[string]string{"PHONEBORG_URL": ts.URL, "PHONEBORG_ADMIN_TOKEN": "tok"}
	stdout, stderr, code := pbctl(t, env, "opencode", "init", "-dir", dir)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "agents: added 4") {
		t.Errorf("stdout: %s", stdout)
	}
}
