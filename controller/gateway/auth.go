// Package gateway is the cluster's OpenAI-compatible inference entry point.
// Authentication, backend selection and backend discovery are interfaces so
// API keys/quotas, scheduling policy and transport can evolve independently
// (see docs/DECISIONS.md ADR-006).
package gateway

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var ErrUnauthorized = errors.New("invalid or missing API key")

// Principal identifies the caller. Name is used for metrics and logs; future
// fields (tenant, quota tier, allowed models) belong here.
type Principal struct {
	Name string
}

type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

// Anonymous is the principal of unauthenticated callers.
const Anonymous = "anonymous"

// AllowAll is for development on a trusted network only.
type AllowAll struct{}

func (AllowAll) Authenticate(*http.Request) (Principal, error) {
	return Principal{Name: Anonymous}, nil
}

// StaticKeys authenticates `Authorization: Bearer <key>` (or `x-api-key`)
// against a set of API keys. Only SHA-256 hashes are kept in memory and
// written to disk. Keys can be created and revoked at runtime; when the store
// has a file, every change is persisted to it atomically.
//
// A store is either enforcing (unknown or missing keys are rejected) or open
// (they are served as "anonymous"; known keys still get their own principal).
// Open stores can be switched to enforcing, never back (see ADR-008).
type StaticKeys struct {
	path     string // empty = in memory only
	enforced atomic.Bool
	now      func() time.Time

	mu      sync.RWMutex
	entries []KeyEntry
	byHash  map[string]string // sha256(key) -> principal name
}

// KeyEntry is one key as stored: its owner, the hex SHA-256 of the key and,
// when known, when it was created.
type KeyEntry struct {
	Name    string
	Hash    string
	Created time.Time // zero for legacy entries
}

var (
	ErrKeyExists  = errors.New("a key with this name already exists")
	ErrKeyUnknown = errors.New("no key with this name")
	ErrKeyName    = errors.New("key name must be 1-64 characters of [A-Za-z0-9._-] and not a reserved name")
)

var keyNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidKeyName reports whether name can own a key. Names end up in files,
// logs and metric labels, so they are restricted to a safe alphabet.
func ValidKeyName(name string) bool {
	return keyNameRE.MatchString(name) && name != Anonymous && name != "admin"
}

func hashKey(k string) string {
	h := sha256.Sum256([]byte(k))
	return hex.EncodeToString(h[:])
}

const hashPrefix = "sha256:"

// NewStaticKeys returns an empty, open store. path may be empty (keys live in
// memory only).
func NewStaticKeys(path string) *StaticKeys {
	return &StaticKeys{path: path, now: time.Now, byHash: map[string]string{}}
}

// LoadKeysFile reads an enforcing store from path. Lines are
//
//	<name> <key>                        legacy plaintext key
//	<name> sha256:<hex> [<RFC 3339>]    hashed key, optional creation time
//
// Blank lines and # comments are ignored. A file without keys is valid and
// rejects every request.
func LoadKeysFile(path string) (*StaticKeys, error) {
	entries, err := readKeysFile(path)
	if err != nil {
		return nil, err
	}
	sk := NewStaticKeys(path)
	sk.enforced.Store(true)
	sk.set(entries)
	return sk, nil
}

func readKeysFile(path string) ([]KeyEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []KeyEntry
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		e, ok, err := parseKeyLine(sc.Text())
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		if ok {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

func parseKeyLine(line string) (KeyEntry, bool, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return KeyEntry{}, false, nil
	}
	f := strings.Fields(line)
	var hexHash string
	hashed := false
	if len(f) >= 2 {
		hexHash, hashed = strings.CutPrefix(f[1], hashPrefix)
	}
	if !hashed {
		if len(f) != 2 {
			return KeyEntry{}, false, errors.New(`want "<name> <key>" or "<name> sha256:<hex> [created]"`)
		}
		return KeyEntry{Name: f[0], Hash: hashKey(f[1])}, true, nil
	}
	if len(f) > 3 {
		return KeyEntry{}, false, errors.New(`want "<name> sha256:<hex> [created]"`)
	}
	if b, err := hex.DecodeString(hexHash); err != nil || len(b) != sha256.Size {
		return KeyEntry{}, false, errors.New("sha256: needs 64 hex characters")
	}
	e := KeyEntry{Name: f[0], Hash: strings.ToLower(hexHash)}
	if len(f) == 3 {
		t, err := time.Parse(time.RFC3339, f[2])
		if err != nil {
			return KeyEntry{}, false, fmt.Errorf("created time: %w", err)
		}
		e.Created = t
	}
	return e, true, nil
}

// set replaces the entries. Must not hold mu.
func (s *StaticKeys) set(entries []KeyEntry) {
	byHash := make(map[string]string, len(entries))
	for _, e := range entries {
		byHash[e.Hash] = e.Name
	}
	s.mu.Lock()
	s.entries, s.byHash = entries, byHash
	s.mu.Unlock()
}

func (s *StaticKeys) Authenticate(r *http.Request) (Principal, error) {
	key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		key = r.Header.Get("x-api-key")
	}
	key = strings.TrimSpace(key)
	s.mu.RLock()
	name, found := s.byHash[hashKey(key)]
	s.mu.RUnlock()
	if found && key != "" {
		return Principal{Name: name}, nil
	}
	if !s.enforced.Load() {
		return Principal{Name: Anonymous}, nil
	}
	return Principal{}, ErrUnauthorized
}

// Enforced reports whether requests without a valid key are rejected.
func (s *StaticKeys) Enforced() bool { return s.enforced.Load() }

// Enforce makes the store reject requests without a valid key. It refuses an
// empty store, which would lock every client out.
func (s *StaticKeys) Enforce() error {
	s.mu.RLock()
	n := len(s.entries)
	s.mu.RUnlock()
	if n == 0 && !s.enforced.Load() {
		return errors.New("create an API key before enforcing key authentication")
	}
	s.enforced.Store(true)
	return nil
}

// Path is the backing file, empty when keys are kept in memory only.
func (s *StaticKeys) Path() string { return s.path }

// List returns the entries sorted by name.
func (s *StaticKeys) List() []KeyEntry {
	s.mu.RLock()
	out := append([]KeyEntry(nil), s.entries...)
	s.mu.RUnlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Create generates a new key for name, persists its hash and returns the
// plaintext key. The plaintext is not kept anywhere.
func (s *StaticKeys) Create(name string) (string, KeyEntry, error) {
	if !ValidKeyName(name) {
		return "", KeyEntry{}, ErrKeyName
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", KeyEntry{}, err
	}
	key := "pb-" + base64.RawURLEncoding.EncodeToString(b)
	e := KeyEntry{Name: name, Hash: hashKey(key), Created: s.now().UTC().Truncate(time.Second)}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.entries {
		if x.Name == name {
			return "", KeyEntry{}, ErrKeyExists
		}
	}
	next := append(append([]KeyEntry(nil), s.entries...), e)
	if err := s.persist(next); err != nil {
		return "", KeyEntry{}, err
	}
	s.entries = next
	s.byHash[e.Hash] = name
	return key, e, nil
}

// Revoke removes every key of name.
func (s *StaticKeys) Revoke(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var next []KeyEntry
	for _, e := range s.entries {
		if e.Name != name {
			next = append(next, e)
		}
	}
	if len(next) == len(s.entries) {
		return ErrKeyUnknown
	}
	if err := s.persist(next); err != nil {
		return err
	}
	s.entries = next
	s.byHash = map[string]string{}
	for _, e := range next {
		s.byHash[e.Hash] = e.Name
	}
	return nil
}

// Reload re-reads the backing file (e.g. on SIGHUP). On error the current
// keys stay in effect. A store without a file has nothing to reload.
func (s *StaticKeys) Reload() error {
	if s.path == "" {
		return nil
	}
	entries, err := readKeysFile(s.path)
	if err != nil {
		return err
	}
	s.set(entries)
	return nil
}

// persist writes entries to the backing file atomically (temp file + rename,
// mode 0600). Legacy plaintext entries are written back as hashes. Must hold mu.
func (s *StaticKeys) persist(entries []KeyEntry) error {
	if s.path == "" {
		return nil
	}
	var b strings.Builder
	b.WriteString("# PhoneBorg API keys: \"<name> sha256:<hex> [created]\" or legacy \"<name> <key>\".\n")
	b.WriteString("# Managed by the controller (pbctl keys); only hashes are stored.\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "%s %s%s", e.Name, hashPrefix, e.Hash)
		if !e.Created.IsZero() {
			fmt.Fprintf(&b, " %s", e.Created.UTC().Format(time.RFC3339))
		}
		b.WriteString("\n")
	}
	return WriteFileAtomic(s.path, []byte(b.String()), 0o600)
}

// WriteFileAtomic writes data to a temp file next to path, syncs it and
// renames it over path, so readers never see a partial file.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
