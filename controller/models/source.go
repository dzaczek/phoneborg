package models

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// ErrInvalid marks a request error (HTTP 400).
var ErrInvalid = errors.New("invalid request")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// HuggingFaceBase is where hf:// sources are fetched from.
const HuggingFaceBase = "https://huggingface.co"

var (
	idRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	shardRe = regexp.MustCompile(`(?i)-\d{5}-of-\d{5}\.gguf$`)
)

// Source is a resolved model source.
type Source struct {
	URL  string // http(s) URL, or empty for a local file
	Path string // local file (file:// source)
	File string // file name, e.g. "qwen2.5-1.5b-instruct-q4_k_m.gguf"
}

// ResolveSource accepts
//
//	https://huggingface.co/<owner>/<repo>/resolve/main/<file>.gguf (any http(s) URL)
//	hf://<owner>/<repo>/<file>.gguf
//	file:///abs/path.gguf
//
// hfBase replaces HuggingFaceBase (tests).
func ResolveSource(src, hfBase string) (Source, error) {
	u, err := url.Parse(strings.TrimSpace(src))
	if err != nil {
		return Source{}, invalid("source: %v", err)
	}
	var s Source
	switch u.Scheme {
	case "https", "http":
		if u.Host == "" {
			return Source{}, invalid("source %q has no host", src)
		}
		s.URL, s.File = u.String(), path.Base(u.Path)
	case "hf":
		// hf://owner/repo/file.gguf parses with Host=owner, Path=/repo/file.gguf.
		repo, file, ok := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
		if u.Host == "" || !ok || repo == "" || file == "" {
			return Source{}, invalid("source %q: want hf://<owner>/<repo>/<file>.gguf", src)
		}
		s.URL = strings.TrimRight(hfBase, "/") + "/" + u.Host + "/" + repo + "/resolve/main/" + file
		s.File = path.Base(file)
	case "file":
		if u.Host != "" || !path.IsAbs(u.Path) {
			return Source{}, invalid("source %q: want file:///absolute/path.gguf", src)
		}
		s.Path, s.File = path.Clean(u.Path), path.Base(u.Path)
	default:
		return Source{}, invalid("source %q: scheme must be https, hf or file", src)
	}
	if !strings.HasSuffix(strings.ToLower(s.File), ".gguf") {
		return Source{}, invalid("source %q is not a .gguf file", src)
	}
	if shardRe.MatchString(s.File) {
		return Source{}, invalid("source %q is one part of a split GGUF; split models are not supported", src)
	}
	return s, nil
}

// Slug turns a file name into a model id: lowercase, without .gguf, and
// only [a-z0-9._-]. "Qwen2.5-0.5B-Instruct-Q4_K_M.gguf" becomes
// "qwen2.5-0.5b-instruct-q4_k_m".
func Slug(file string) string {
	s := strings.ToLower(file)
	s = strings.TrimSuffix(s, ".gguf")
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-._")
}

// ValidID reports whether id is a usable model id.
func ValidID(id string) bool { return idRe.MatchString(id) }
