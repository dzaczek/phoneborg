package controller

import (
	"net/http"

	"github.com/dzaczek/phoneborg/controller/gateway"
)

// Admin API behind the web panel's "Chat test" view (ADR-018): a way for an
// operator to exercise the gateway from a browser that has only the admin
// token, notably a remote browser against a controller run with
// -gateway-access local (ADR-017), where the browser is neither a trusted
// peer nor holds an API key.

// PanelPrincipal is the usage/metrics identity for gateway requests sent
// through the admin-authenticated chat endpoint, distinct from "anonymous",
// "local" and any API key owner.
const PanelPrincipal = "panel"

func (s *Server) registerChatAdmin(add func(pattern, action string, fn http.HandlerFunc)) {
	add("GET /admin/chat/models", "chat_models", s.adminChatModels)
	add("POST /admin/chat/completions", "chat_completions", s.adminChatCompletions)
}

// adminChatModels lists the same served and virtual models as GET
// /v1/models (ADR-014: auto, pool/<name>, node/<alias-or-id>, concrete
// models), for the panel's model selector; the admin token is not
// necessarily a valid gateway API key, so the panel cannot always reach
// /v1/models directly (ADR-017).
func (s *Server) adminChatModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		Object string               `json:"object"`
		Data   []gateway.ModelEntry `json:"data"`
	}{Object: "list", Data: s.gw.ModelEntries()})
}

// adminChatCompletions proxies a chat completion through the normal gateway
// path (routing, failover, affinity, usage and metrics), exactly as
// POST /v1/chat/completions would, but already authenticated by the admin
// token (adminAuth) and attributed to PanelPrincipal instead of an API key
// or -gateway-access principal. Streaming (SSE) passes through unchanged.
func (s *Server) adminChatCompletions(w http.ResponseWriter, r *http.Request) {
	r.URL.Path = "/v1/chat/completions" // so the gateway forwards to the right upstream path
	s.gw.ServeChat(w, r, gateway.Principal{Name: PanelPrincipal})
}
