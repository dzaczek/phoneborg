package gateway

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func mustCIDRs(t *testing.T, csv string) []*net.IPNet {
	t.Helper()
	nets, err := ParseCIDRList(csv)
	if err != nil {
		t.Fatalf("ParseCIDRList(%q): %v", csv, err)
	}
	return nets
}

func TestParseCIDRList(t *testing.T) {
	if got := mustCIDRs(t, ""); got != nil {
		t.Errorf("empty string: got %v, want nil", got)
	}
	got := mustCIDRs(t, "127.0.0.0/8, ::1/128")
	if len(got) != 2 || !got[0].Contains(net.ParseIP("127.0.0.1")) || !got[1].Contains(net.ParseIP("::1")) {
		t.Errorf("parsed = %v", got)
	}
	if _, err := ParseCIDRList("not-a-cidr"); err == nil {
		t.Error("want an error for an invalid CIDR")
	}
	if _, err := ParseCIDRList("127.0.0.0/8,nope"); err == nil {
		t.Error("want an error when one of several CIDRs is invalid")
	}
}

func req(remoteAddr string, hdr ...string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = remoteAddr
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	return r
}

var loopback = mustCIDRsPkg("127.0.0.0/8,::1/128")

// mustCIDRsPkg is mustCIDRs without a *testing.T, for package-level vars.
func mustCIDRsPkg(csv string) []*net.IPNet {
	nets, err := ParseCIDRList(csv)
	if err != nil {
		panic(err)
	}
	return nets
}

func TestAccessControlLocalMode(t *testing.T) {
	keys := NewStaticKeys("")
	key, _, err := keys.Create("alice")
	if err != nil {
		t.Fatal(err)
	}
	ac := AccessControl{Keys: keys, Mode: AccessLocal, Trusted: loopback}

	if p, err := ac.Authenticate(req("127.0.0.1:5555")); err != nil || p.Name != Local {
		t.Errorf("loopback, no key: principal=%+v err=%v, want Local, nil", p, err)
	}
	if p, err := ac.Authenticate(req("[::1]:5555")); err != nil || p.Name != Local {
		t.Errorf("IPv6 loopback: principal=%+v err=%v, want Local, nil", p, err)
	}
	if _, err := ac.Authenticate(req("203.0.113.9:5555")); err == nil {
		t.Error("remote peer, no key: want an error")
	} else if err != ErrRemoteRequiresKey {
		t.Errorf("remote peer, no key: err=%v, want ErrRemoteRequiresKey", err)
	}
	if p, err := ac.Authenticate(req("203.0.113.9:5555", "Authorization", "Bearer "+key)); err != nil || p.Name != "alice" {
		t.Errorf("remote peer, valid key: principal=%+v err=%v, want alice, nil", p, err)
	}
	if _, err := ac.Authenticate(req("203.0.113.9:5555", "Authorization", "Bearer wrong")); err != ErrRemoteRequiresKey {
		t.Errorf("remote peer, invalid key: err=%v, want ErrRemoteRequiresKey", err)
	}
	// A valid key from a trusted peer is still attributed to its owner, not "local".
	if p, err := ac.Authenticate(req("127.0.0.1:5555", "x-api-key", key)); err != nil || p.Name != "alice" {
		t.Errorf("loopback with a key: principal=%+v err=%v, want alice, nil", p, err)
	}
}

func TestAccessControlKeysMode(t *testing.T) {
	keys := NewStaticKeys("")
	key, _, _ := keys.Create("alice")
	ac := AccessControl{Keys: keys, Mode: AccessKeys, Trusted: loopback}

	if _, err := ac.Authenticate(req("127.0.0.1:5555")); err != ErrUnauthorized {
		t.Errorf("loopback, no key: err=%v, want ErrUnauthorized (keys mode ignores trust)", err)
	}
	if p, err := ac.Authenticate(req("127.0.0.1:5555", "Authorization", "Bearer "+key)); err != nil || p.Name != "alice" {
		t.Errorf("loopback, valid key: principal=%+v err=%v", p, err)
	}
}

func TestAccessControlOpenMode(t *testing.T) {
	keys := NewStaticKeys("")
	key, _, _ := keys.Create("alice")
	ac := AccessControl{Keys: keys, Mode: AccessOpen, Trusted: loopback}

	if p, err := ac.Authenticate(req("203.0.113.9:5555")); err != nil || p.Name != Anonymous {
		t.Errorf("remote, no key: principal=%+v err=%v, want anonymous, nil", p, err)
	}
	if p, err := ac.Authenticate(req("203.0.113.9:5555", "Authorization", "Bearer "+key)); err != nil || p.Name != "alice" {
		t.Errorf("remote, valid key: principal=%+v err=%v, want alice, nil", p, err)
	}
}

// TestAccessControlEnforcedKeysOverridesMode checks the compatibility rule:
// once StaticKeys.Enforce() is called (the runtime `pbctl gateway set
// auth=keys` switch), "local" and "open" modes behave like "keys" (ADR-017).
func TestAccessControlEnforcedKeysOverridesMode(t *testing.T) {
	keys := NewStaticKeys("")
	key, _, _ := keys.Create("alice")
	if err := keys.Enforce(); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{AccessLocal, AccessOpen} {
		ac := AccessControl{Keys: keys, Mode: mode, Trusted: loopback}
		if _, err := ac.Authenticate(req("127.0.0.1:5555")); err != ErrUnauthorized {
			t.Errorf("mode=%s, loopback, no key: err=%v, want ErrUnauthorized once keys are enforced", mode, err)
		}
		if p, err := ac.Authenticate(req("203.0.113.9:5555", "Authorization", "Bearer "+key)); err != nil || p.Name != "alice" {
			t.Errorf("mode=%s, remote, valid key: principal=%+v err=%v", mode, p, err)
		}
	}
}

func TestAccessControlXForwardedFor(t *testing.T) {
	keys := NewStaticKeys("")
	proxyCIDR := mustCIDRs(t, "203.0.113.0/24")
	ac := AccessControl{Keys: keys, Mode: AccessLocal, Trusted: loopback, TrustedProxies: proxyCIDR}

	// The proxy (203.0.113.5) is trusted, so the forwarded client address is used.
	if p, err := ac.Authenticate(req("203.0.113.5:443", "X-Forwarded-For", "127.0.0.1")); err != nil || p.Name != Local {
		t.Errorf("trusted proxy forwarding a loopback client: principal=%+v err=%v, want Local, nil", p, err)
	}
	if _, err := ac.Authenticate(req("203.0.113.5:443", "X-Forwarded-For", "198.51.100.7")); err != ErrRemoteRequiresKey {
		t.Errorf("trusted proxy forwarding a remote client: err=%v, want ErrRemoteRequiresKey", err)
	}
	// A direct peer that is NOT a trusted proxy cannot claim to be loopback via XFF.
	if _, err := ac.Authenticate(req("198.51.100.7:443", "X-Forwarded-For", "127.0.0.1")); err != ErrRemoteRequiresKey {
		t.Errorf("untrusted peer spoofing X-Forwarded-For: err=%v, want ErrRemoteRequiresKey", err)
	}
}

func doWithAddr(h http.Handler, method, path, body, remoteAddr string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = remoteAddr
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// TestGatewayAccessControlEndToEnd exercises AccessControl through the real
// HTTP handlers (not just Authenticate directly), on both /v1/chat/completions
// and /v1/models, and checks the rejection metric and 403 body (ADR-017).
func TestGatewayAccessControlEndToEnd(t *testing.T) {
	a := fakeLlama(t, "a")
	keys := NewStaticKeys("")
	key, _, _ := keys.Create("alice")
	ac := AccessControl{Keys: keys, Mode: AccessLocal, Trusted: loopback}
	g, h := newGW(t, ac, Backend{NodeID: "a", Model: "m", URL: a.URL})

	if w := doWithAddr(h, http.MethodPost, "/v1/chat/completions", chat, "127.0.0.1:5555"); w.Code != http.StatusOK {
		t.Fatalf("loopback: %d %s", w.Code, w.Body)
	}
	w := doWithAddr(h, http.MethodPost, "/v1/chat/completions", chat, "203.0.113.9:5555")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "remote_requires_api_key") {
		t.Fatalf("remote, no key: %d %s", w.Code, w.Body)
	}
	if got := testutil.ToFloat64(g.mRejected.WithLabelValues("remote_requires_api_key")); got != 1 {
		t.Errorf("rejected metric = %v, want 1", got)
	}
	if w := doWithAddr(h, http.MethodPost, "/v1/chat/completions", chat, "203.0.113.9:5555", "Authorization", "Bearer "+key); w.Code != http.StatusOK {
		t.Fatalf("remote with a valid key: %d %s", w.Code, w.Body)
	}
	if w := doWithAddr(h, http.MethodGet, "/v1/models", "", "203.0.113.9:5555"); w.Code != http.StatusForbidden {
		t.Fatalf("/v1/models from a remote peer: %d %s", w.Code, w.Body)
	}
	if w := doWithAddr(h, http.MethodGet, "/v1/models", "", "127.0.0.1:5555"); w.Code != http.StatusOK {
		t.Fatalf("/v1/models from loopback: %d %s", w.Code, w.Body)
	}
}

func TestStaticKeysLookupIgnoresEnforced(t *testing.T) {
	keys := NewStaticKeys("")
	key, _, _ := keys.Create("alice")
	// Lookup finds a valid key whether or not the store is enforced.
	if p, ok := keys.Lookup(req("1.2.3.4:1", "Authorization", "Bearer "+key)); !ok || p.Name != "alice" {
		t.Errorf("Lookup (open store) = %+v, %v", p, ok)
	}
	if _, ok := keys.Lookup(req("1.2.3.4:1")); ok {
		t.Error("Lookup with no key should be ok=false")
	}
	if _, ok := keys.Lookup(req("1.2.3.4:1", "Authorization", "Bearer wrong")); ok {
		t.Error("Lookup with an unknown key should be ok=false")
	}
}
