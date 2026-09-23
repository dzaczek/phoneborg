// Package gateway is the cluster's OpenAI-compatible inference entry point.
// Authentication, backend selection and backend discovery are interfaces so
// API keys/quotas, scheduling policy and transport can evolve independently
// (see docs/DECISIONS.md ADR-006).
package gateway

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
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

// AllowAll is for development on a trusted network only.
type AllowAll struct{}

func (AllowAll) Authenticate(*http.Request) (Principal, error) {
	return Principal{Name: "anonymous"}, nil
}

// StaticKeys authenticates `Authorization: Bearer <key>` (or `x-api-key`)
// against keys loaded at startup. Only SHA-256 hashes are kept in memory.
type StaticKeys struct {
	byHash map[string]string // sha256(key) -> principal name
}

func hashKey(k string) string {
	h := sha256.Sum256([]byte(k))
	return hex.EncodeToString(h[:])
}

// LoadKeysFile reads lines of "<name> <key>"; blank lines and # comments are
// ignored.
func LoadKeysFile(path string) (*StaticKeys, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sk := &StaticKeys{byHash: map[string]string{}}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 {
			return nil, fmt.Errorf("%s:%d: want \"<name> <key>\"", path, n)
		}
		sk.byHash[hashKey(f[1])] = f[0]
	}
	if len(sk.byHash) == 0 {
		return nil, fmt.Errorf("%s: no keys", path)
	}
	return sk, sc.Err()
}

func (s *StaticKeys) Authenticate(r *http.Request) (Principal, error) {
	key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		key = r.Header.Get("x-api-key")
	}
	if name, found := s.byHash[hashKey(strings.TrimSpace(key))]; found && key != "" {
		return Principal{Name: name}, nil
	}
	return Principal{}, ErrUnauthorized
}
