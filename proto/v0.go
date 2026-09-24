// Package proto defines the controller <-> node wire types.
//
// v0 is JSON over HTTP (see docs/DECISIONS.md ADR-002). The field set is kept
// protobuf-friendly (flat, explicit units) so it can move to Protobuf/gRPC
// with mTLS without changing semantics.
package proto

import "time"

const (
	PathRegister  = "/v1/register"
	PathBenchmark = "/v1/benchmark"
	PathHeartbeat = "/v1/heartbeat"
	PathNodes     = "/v1/nodes"
)

// Inventory is discovered on the device at runtime. Nothing here is hard-coded
// per phone model.
type Inventory struct {
	Manufacturer     string `json:"manufacturer"`
	Model            string `json:"model"`
	SoC              string `json:"soc"`
	ABI              string `json:"abi"`
	AndroidRelease   string `json:"android_release"`
	SDK              int    `json:"sdk"`
	CPUCores         int    `json:"cpu_cores"`       // effective cores, respects cgroup cpu.max
	RAMTotalBytes    uint64 `json:"ram_total_bytes"` // min(MemTotal, cgroup memory.max)
	StorageFreeBytes uint64 `json:"storage_free_bytes"`
	AgentVersion     string `json:"agent_version"`
}

// Benchmark is a self-reported capability score. Kind identifies the method so
// scores from different methods are never compared directly.
type Benchmark struct {
	Kind             string  `json:"kind"`
	CPUGFLOPS        float64 `json:"cpu_gflops"`
	MemBandwidthGBps float64 `json:"mem_bandwidth_gbps"`
	DurationMs       int64   `json:"duration_ms"`
}

type RegisterRequest struct {
	NodeID    string    `json:"node_id"`
	Inventory Inventory `json:"inventory"`
}

type RegisterResponse struct {
	NodeID               string `json:"node_id"`
	HeartbeatIntervalSec int    `json:"heartbeat_interval_sec"`
}

type BenchmarkReport struct {
	NodeID    string    `json:"node_id"`
	Benchmark Benchmark `json:"benchmark"`
}

// Heartbeat carries fast-changing runtime state. Pointer fields are nil when
// the device does not expose the value (e.g. no readable thermal zones).
type Heartbeat struct {
	NodeID        string   `json:"node_id"`
	RAMAvailBytes uint64   `json:"ram_avail_bytes"`
	Load1         float64  `json:"load1"`
	TemperatureC  *float64 `json:"temperature_c,omitempty"`
	BatteryLevel  *int     `json:"battery_level,omitempty"`
	UptimeSec     int64    `json:"uptime_sec"`
	// Runtime is nil when the node serves no model.
	Runtime *RuntimeStatus `json:"runtime,omitempty"`
}

// HeartbeatResponse is the body of a 200 reply to POST /v1/heartbeat. A
// controller may still answer 204 (no desired state); agents must accept
// both (see ADR-012).
type HeartbeatResponse struct {
	Desired *DesiredRuntime `json:"desired,omitempty"` // nil = keep current runtime
}

// DesiredRuntime is what the controller wants the node to serve.
type DesiredRuntime struct {
	ModelID   string `json:"model_id"` // catalog id; also the served model name (llama-server -a)
	URL       string `json:"url"`      // path on the controller, e.g. "/v1/model-files/<model_id>"
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	CtxSize   int    `json:"ctx_size"` // requested context per slot; agent may lower it to fit RAM
	Slots     int    `json:"slots"`    // requested parallel slots (llama-server -np)
	KVType    string `json:"kv_type"`  // "f16" | "q8_0" | "auto" (auto: agent picks)
	Layers    int    `json:"layers"`   // GGUF metadata for the RAM estimate
	KVHeads   int    `json:"kv_heads"`
	HeadDim   int    `json:"head_dim"`
}

// RuntimeStatus describes the node's inference server. The controller reaches
// it at AdvertiseHost:AdvertisePort; an empty host means "the controller's
// configured backend host" (the machine running adb, see ADR-003/ADR-006).
type RuntimeStatus struct {
	Engine        string `json:"engine"`
	Model         string `json:"model"`
	Ready         bool   `json:"ready"`
	AdvertiseHost string `json:"advertise_host,omitempty"`
	AdvertisePort int    `json:"advertise_port"`
	Restarts      int64  `json:"restarts"`
	Threads       int    `json:"threads"`
	CtxSize       int    `json:"ctx_size,omitempty"` // llama-server context window in tokens
	// GenTPS and PromptTPS come from a self-test the agent runs against its
	// own llama-server once it becomes ready (and again after each restart):
	// a fixed prompt, read from the response's `timings`. Zero means the
	// self-test has not completed yet or failed (see ADR-010). Unlike
	// Benchmark.CPUGFLOPS (a synthetic score measured before llama-server
	// starts), these are real llama.cpp tokens/s.
	GenTPS           float64   `json:"gen_tps,omitempty"`
	PromptTPS        float64   `json:"prompt_tps,omitempty"`
	SelfTestAt       time.Time `json:"self_test_at,omitempty"`
	ModelID          string    `json:"model_id,omitempty"`
	State            string    `json:"state,omitempty"`    // "serving" | "downloading" | "loading" | "error" | "idle"
	Progress         float64   `json:"progress,omitempty"` // 0..1 while downloading
	Error            string    `json:"error,omitempty"`
	Slots            int       `json:"slots,omitempty"`
	KVType           string    `json:"kv_type,omitempty"`
	RAMEstimateBytes int64     `json:"ram_estimate_bytes,omitempty"`
	// BudgetBytes is the agent's current memory budget (ADR-012's
	// MemoryBudget): what PlanMemory would have available if it switched
	// models right now. Reported every heartbeat whenever the agent has a
	// runtime configured, even before it serves a model, so the placement
	// planner can check fit against a node's real headroom instead of the
	// class heuristic.
	BudgetBytes int64 `json:"budget_bytes,omitempty"`
}

type NodeState string

const (
	StateBenchmarking NodeState = "BENCHMARKING"
	StateActive       NodeState = "ACTIVE"
	StateSuspect      NodeState = "SUSPECT" // missed heartbeats
	StateOffline      NodeState = "OFFLINE"
)

var AllStates = []NodeState{StateBenchmarking, StateActive, StateSuspect, StateOffline}

// Node is the controller's view of a node.
type Node struct {
	ID            string     `json:"id"`
	State         NodeState  `json:"state"`
	RemoteAddr    string     `json:"remote_addr"`
	Inventory     Inventory  `json:"inventory"`
	Benchmark     *Benchmark `json:"benchmark,omitempty"`
	LastHeartbeat *Heartbeat `json:"last_heartbeat,omitempty"`
	RegisteredAt  time.Time  `json:"registered_at"`
	LastSeen      time.Time  `json:"last_seen"`
}
