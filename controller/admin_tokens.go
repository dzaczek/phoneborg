package controller

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dzaczek/phoneborg/controller/gateway"
)

// Named admin tokens (ADR-026). The token from -admin-token-file stays and
// is called "admin" (pcprov and scripts read it from that file); further
// tokens, one per operator or device, live as SHA-256 hashes in
// -admin-tokens-file. The audit log and the admin metrics name the token a
// request used.

// PrimaryAdminToken is the name of the -admin-token-file token.
const PrimaryAdminToken = "admin"

// Admin token errors.
var (
	ErrAdminTokenName    = errors.New("token name must match ^[a-z0-9][a-z0-9-]{0,31}$")
	ErrAdminTokenExists  = errors.New("a token with this name exists")
	ErrAdminTokenUnknown = errors.New("no such token")
	ErrAdminTokenPrimary = errors.New(`the "admin" token comes from -admin-token-file and cannot be revoked here`)
)

// NamedAdminToken is a stored admin token: its hash, never the token.
type NamedAdminToken struct {
	Hash    [sha256.Size]byte
	Created time.Time
}

// AdminToken is one token in GET /admin/tokens.
type AdminToken struct {
	Name    string    `json:"name"`
	Created time.Time `json:"created,omitzero"`
	Primary bool      `json:"primary"` // the -admin-token-file token
}

// AdminTokens is the response of GET /admin/tokens.
type AdminTokens struct {
	Persisted bool         `json:"persisted"` // new tokens are written to -admin-tokens-file
	Tokens    []AdminToken `json:"tokens"`
}

// AdminTokenCreate is the body of POST /admin/tokens.
type AdminTokenCreate struct {
	Name string `json:"name"`
}

// CreatedAdminToken is the response of POST /admin/tokens; Token is shown
// only once.
type CreatedAdminToken struct {
	Name      string    `json:"name"`
	Token     string    `json:"token"`
	Created   time.Time `json:"created"`
	Persisted bool      `json:"persisted"`
}

// LoadAdminTokens reads a named-token file: "<name> sha256:<hex>
// [created RFC 3339]" lines, # comments allowed. A missing file is no tokens.
func LoadAdminTokens(path string) (map[string]NamedAdminToken, error) {
	out := map[string]NamedAdminToken{}
	if path == "" {
		return out, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		hexHash, ok := strings.CutPrefix(safeField(fields, 1), "sha256:")
		raw, err := hex.DecodeString(hexHash)
		if len(fields) < 2 || !ok || err != nil || len(raw) != sha256.Size || !nameRE.MatchString(fields[0]) || fields[0] == PrimaryAdminToken {
			return nil, fmt.Errorf("%s:%d: want \"<name> sha256:<hex> [created]\"", path, n)
		}
		var t NamedAdminToken
		copy(t.Hash[:], raw)
		if len(fields) > 2 {
			t.Created, _ = time.Parse(time.RFC3339, fields[2])
		}
		out[fields[0]] = t
	}
	return out, sc.Err()
}

func safeField(f []string, i int) string {
	if i < len(f) {
		return f[i]
	}
	return ""
}

func newAdminTokens(o AdminOptions) *adminTokens {
	named := map[string]NamedAdminToken{}
	for n, t := range o.Tokens {
		named[n] = t
	}
	return &adminTokens{path: o.TokensFile, primary: sha256.Sum256([]byte(o.Token)), named: named}
}

// adminTokens holds the primary token's hash and the named tokens.
type adminTokens struct {
	path    string // "" = named tokens live in memory only
	primary [sha256.Size]byte

	mu    sync.RWMutex
	named map[string]NamedAdminToken
}

// match returns the name of the token tok, comparing every hash in
// constant time.
func (t *adminTokens) match(tok string) (string, bool) {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return "", false
	}
	h := sha256.Sum256([]byte(tok))
	name := ""
	if subtle.ConstantTimeCompare(h[:], t.primary[:]) == 1 {
		name = PrimaryAdminToken
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	for n, nt := range t.named {
		if subtle.ConstantTimeCompare(h[:], nt.Hash[:]) == 1 {
			name = n
		}
	}
	return name, name != ""
}

func (t *adminTokens) list() AdminTokens {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := AdminTokens{Persisted: t.path != "", Tokens: []AdminToken{{Name: PrimaryAdminToken, Primary: true}}}
	for n, nt := range t.named {
		out.Tokens = append(out.Tokens, AdminToken{Name: n, Created: nt.Created})
	}
	sort.Slice(out.Tokens[1:], func(i, j int) bool { return out.Tokens[i+1].Name < out.Tokens[j+1].Name })
	return out
}

// create makes a token for name and stores its hash.
func (t *adminTokens) create(name string, now time.Time) (CreatedAdminToken, error) {
	if !nameRE.MatchString(name) {
		return CreatedAdminToken{}, ErrAdminTokenName
	}
	if name == PrimaryAdminToken {
		return CreatedAdminToken{}, ErrAdminTokenExists
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return CreatedAdminToken{}, err
	}
	tok := hex.EncodeToString(b)
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.named[name]; ok {
		return CreatedAdminToken{}, ErrAdminTokenExists
	}
	t.named[name] = NamedAdminToken{Hash: sha256.Sum256([]byte(tok)), Created: now.UTC().Truncate(time.Second)}
	if err := t.saveLocked(); err != nil {
		delete(t.named, name)
		return CreatedAdminToken{}, err
	}
	return CreatedAdminToken{Name: name, Token: tok, Created: t.named[name].Created, Persisted: t.path != ""}, nil
}

func (t *adminTokens) revoke(name string) error {
	if name == PrimaryAdminToken {
		return ErrAdminTokenPrimary
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	old, ok := t.named[name]
	if !ok {
		return ErrAdminTokenUnknown
	}
	delete(t.named, name)
	if err := t.saveLocked(); err != nil {
		t.named[name] = old
		return err
	}
	return nil
}

func (t *adminTokens) saveLocked() error {
	if t.path == "" {
		return nil
	}
	names := make([]string, 0, len(t.named))
	for n := range t.named {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("# PhoneBorg named admin tokens (ADR-026): <name> sha256:<hex> <created>. Managed by the controller.\n")
	for _, n := range names {
		nt := t.named[n]
		fmt.Fprintf(&b, "%s sha256:%s %s\n", n, hex.EncodeToString(nt.Hash[:]), nt.Created.Format(time.RFC3339))
	}
	if err := os.MkdirAll(filepath.Dir(t.path), 0o700); err != nil {
		return err
	}
	return gateway.WriteFileAtomic(t.path, []byte(b.String()), 0o600)
}

// principalKey carries the admin token name in a request's context.
type principalKey struct{}

// adminPrincipal is the name of the admin token that authenticated r.
func adminPrincipal(r *http.Request) string {
	if n, ok := r.Context().Value(principalKey{}).(string); ok {
		return n
	}
	return PrimaryAdminToken
}

func withAdminPrincipal(r *http.Request, name string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, name))
}

// Admin handlers.

func (s *Server) registerTokenAdmin(add func(pattern, action string, fn http.HandlerFunc)) {
	add("GET /admin/tokens", "tokens_list", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.adminTokens.list())
	})
	add("POST /admin/tokens", "token_create", s.adminCreateToken)
	add("DELETE /admin/tokens/{name}", "token_revoke", s.adminRevokeToken)
}

func (s *Server) adminCreateToken(w http.ResponseWriter, r *http.Request) {
	var c AdminTokenCreate
	if !decodeStrict(w, r, &c) {
		return
	}
	tok, err := s.adminTokens.create(strings.TrimSpace(c.Name), time.Now())
	s.audit(r, "token_create", err, "token_name", c.Name)
	switch {
	case errors.Is(err, ErrAdminTokenName):
		httpError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrAdminTokenExists):
		httpError(w, http.StatusConflict, err.Error())
	case err != nil:
		httpError(w, http.StatusInternalServerError, "cannot store the token: "+err.Error())
	default:
		writeJSON(w, http.StatusCreated, tok)
	}
}

func (s *Server) adminRevokeToken(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := s.adminTokens.revoke(name)
	s.audit(r, "token_revoke", err, "token_name", name)
	switch {
	case errors.Is(err, ErrAdminTokenPrimary):
		httpError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrAdminTokenUnknown):
		httpError(w, http.StatusNotFound, err.Error())
	case err != nil:
		httpError(w, http.StatusInternalServerError, "cannot store the tokens: "+err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
