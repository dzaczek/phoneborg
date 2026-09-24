package models

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dzaczek/phoneborg/controller/gateway"
)

// Model statuses.
const (
	StatusDownloading = "downloading"
	StatusReady       = "ready"
	StatusError       = "error"
)

var (
	ErrUnknownModel = errors.New("unknown model")
	ErrExists       = errors.New("model already exists")
)

// Model is a catalog entry, as returned by the admin API. Default and
// NodesServing are filled in by the controller, which owns placement and
// node state.
type Model struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Source             string   `json:"source"`
	File               string   `json:"file"`
	SizeBytes          int64    `json:"size_bytes"`
	SHA256             string   `json:"sha256"`
	Status             string   `json:"status"`
	Progress           float64  `json:"progress"`
	Error              string   `json:"error"`
	Arch               string   `json:"arch"`
	Params             string   `json:"params"`
	Quant              string   `json:"quant"`
	CtxTrain           int      `json:"ctx_train"`
	Layers             int      `json:"layers"`
	KVHeads            int      `json:"kv_heads"`
	HeadDim            int      `json:"head_dim"`
	EstRAMBytes16k     int64    `json:"est_ram_bytes_16k"`
	FitsClasses        []string `json:"fits_classes"`
	Tags               []string `json:"tags"`
	RecommendedClasses []string `json:"recommended_classes"`
	License            string   `json:"license"`
	Default            bool     `json:"default"`
	NodesServing       int      `json:"nodes_serving"`
}

// PlanModel returns m as the planner sees it.
func (m Model) PlanModel() PlanModel {
	return PlanModel{ID: m.ID, SizeBytes: m.SizeBytes, CtxTrain: m.CtxTrain, Layers: m.Layers, KVHeads: m.KVHeads, HeadDim: m.HeadDim}
}

// AddRequest is the body of POST /admin/models.
type AddRequest struct {
	Source             string   `json:"source"`
	ID                 string   `json:"id,omitempty"`
	Name               string   `json:"name,omitempty"`
	Tags               []string `json:"tags,omitempty"`
	RecommendedClasses []string `json:"recommended_classes,omitempty"`
}

// Patch is the body of PATCH /admin/models/{id}; nil (or JSON null) fields
// are unchanged, an empty list clears. Default is applied by the controller
// (it is part of placement).
type Patch struct {
	Name               *string  `json:"name,omitempty"`
	Tags               []string `json:"tags"`
	RecommendedClasses []string `json:"recommended_classes"`
	Default            *bool    `json:"default,omitempty"`
}

// CatalogOptions configures a Catalog.
type CatalogOptions struct {
	// Dir holds the model files. Empty = a temporary directory created on
	// first use (files are lost on restart).
	Dir string
	// StateFile persists the catalog (JSON). Empty = memory only.
	StateFile string
	// HTTPClient downloads models; nil = a client without an overall
	// timeout (models are gigabytes) but with a response header timeout.
	HTTPClient *http.Client
	// HuggingFaceBase replaces HuggingFaceBase for hf:// sources (tests).
	HuggingFaceBase string
	// IdleTimeout aborts a download attempt that receives nothing for this
	// long; 0 = 2 minutes.
	IdleTimeout time.Duration
	// RetryDelay is the pause before retrying a failed download; 0 = 5 s.
	RetryDelay time.Duration
	// OnChange is called (without locks held) when a model is added,
	// becomes ready or fails, or is removed.
	OnChange func()
	Log      *slog.Logger
}

// Catalog keeps model metadata and files, and downloads new models.
type Catalog struct {
	opts CatalogOptions
	log  *slog.Logger

	mu      sync.Mutex
	dir     string
	models  map[string]*entry
	closing bool
	wg      sync.WaitGroup
}

type entry struct {
	m       Model
	cancel  context.CancelFunc
	written atomic.Int64 // bytes of the file so far while downloading
	total   atomic.Int64 // expected size; 0 = unknown
}

type catalogFile struct {
	Version int     `json:"version"`
	Models  []Model `json:"models"`
}

const downloadAttempts = 3

// NewCatalog loads the state file if it exists, marks ready models whose
// file disappeared as errors and resumes unfinished downloads.
func NewCatalog(opts CatalogOptions) (*Catalog, error) {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.OnChange == nil {
		opts.OnChange = func() {}
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment,
			ResponseHeaderTimeout: time.Minute, TLSHandshakeTimeout: 30 * time.Second}}
	}
	if opts.HuggingFaceBase == "" {
		opts.HuggingFaceBase = HuggingFaceBase
	}
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = 2 * time.Minute
	}
	if opts.RetryDelay == 0 {
		opts.RetryDelay = 5 * time.Second
	}
	c := &Catalog{opts: opts, log: opts.Log, dir: opts.Dir, models: map[string]*entry{}}
	if c.dir != "" {
		if err := os.MkdirAll(c.dir, 0o700); err != nil {
			return nil, err
		}
	}
	if opts.StateFile == "" {
		return c, nil
	}
	data, err := os.ReadFile(opts.StateFile)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return c, nil
	case err != nil:
		return nil, err
	}
	var f catalogFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w (move it away to start with an empty catalog)", opts.StateFile, err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported version %d", opts.StateFile, f.Version)
	}
	if c.dir == "" {
		return nil, errors.New("a persisted catalog needs a models directory")
	}
	for _, m := range f.Models {
		e := &entry{m: m}
		c.models[m.ID] = e
		switch m.Status {
		case StatusReady:
			if st, err := os.Stat(c.filePath(m.ID)); err != nil || st.Size() != m.SizeBytes {
				e.m.Status, e.m.Error = StatusError, "model file is missing or changed; add the model again"
				c.log.Warn("model file missing", "model_id", m.ID, "file", c.filePath(m.ID))
			}
		case StatusDownloading:
			c.startLocked(e)
		}
	}
	return c, nil
}

// SetOnChange replaces the OnChange callback.
func (c *Catalog) SetOnChange(f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opts.OnChange = f
}

func (c *Catalog) changed() {
	c.mu.Lock()
	f := c.opts.OnChange
	c.mu.Unlock()
	f()
}

// Dir is the directory with the model files ("" until the first download
// when no directory was configured).
func (c *Catalog) Dir() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dir
}

func (c *Catalog) filePath(id string) string { return filepath.Join(c.dir, id+".gguf") }
func (c *Catalog) partPath(id string) string { return filepath.Join(c.dir, id+".gguf.part") }

// view returns e's model with live download progress. Call with mu held.
func (e *entry) view() Model {
	m := e.m
	m.Tags = nonNil(m.Tags)
	m.RecommendedClasses = nonNil(m.RecommendedClasses)
	m.FitsClasses = nonNil(m.FitsClasses)
	switch m.Status {
	case StatusReady:
		m.Progress = 1
	case StatusDownloading:
		if t := e.total.Load(); t > 0 {
			m.Progress = min(1, float64(e.written.Load())/float64(t))
		}
	}
	return m
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// List returns all models sorted by id.
func (c *Catalog) List() []Model {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Model, 0, len(c.models))
	for _, e := range c.models {
		out = append(out, e.view())
	}
	slices.SortFunc(out, func(a, b Model) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// Get returns one model.
func (c *Catalog) Get(id string) (Model, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.models[id]
	if !ok {
		return Model{}, false
	}
	return e.view(), true
}

// File returns the path of a ready model's file.
func (c *Catalog) File(id string) (string, Model, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.models[id]
	if !ok || e.m.Status != StatusReady {
		return "", Model{}, false
	}
	return c.filePath(id), e.view(), true
}

// Add registers a model and starts downloading it in the background. Adding
// an id whose previous download failed retries it (resuming the partial
// file when the source is unchanged).
func (c *Catalog) Add(req AddRequest) (Model, error) {
	src, err := ResolveSource(req.Source, c.opts.HuggingFaceBase)
	if err != nil {
		return Model{}, err
	}
	id := req.ID
	if id == "" {
		id = Slug(src.File)
	}
	if !ValidID(id) {
		return Model{}, invalid("id %q: use 1-128 of a-z 0-9 . _ - starting with a letter or digit", id)
	}
	tags, err := normTags(req.Tags)
	if err != nil {
		return Model{}, err
	}
	classes, err := normClasses(req.RecommendedClasses)
	if err != nil {
		return Model{}, err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = strings.TrimSuffix(src.File, filepath.Ext(src.File))
	}

	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return Model{}, errors.New("catalog is shutting down")
	}
	if old, ok := c.models[id]; ok {
		if old.m.Status != StatusError {
			c.mu.Unlock()
			return Model{}, fmt.Errorf("%w: %s (%s)", ErrExists, id, old.m.Status)
		}
		if old.m.Source != req.Source && c.dir != "" {
			_ = os.Remove(c.partPath(id)) // a different file: do not resume
		}
	} else if c.dir != "" {
		_ = os.Remove(c.partPath(id)) // left over from a removed model
	}
	if c.dir == "" {
		d, err := os.MkdirTemp("", "phoneborg-models-")
		if err != nil {
			c.mu.Unlock()
			return Model{}, err
		}
		c.dir = d
		c.log.Warn("no models directory configured; model files are kept in a temporary directory and lost on restart", "dir", d)
	}
	e := &entry{m: Model{ID: id, Name: name, Source: req.Source, File: src.File, Status: StatusDownloading,
		Tags: tags, RecommendedClasses: classes}}
	c.models[id] = e
	c.startLocked(e)
	m := e.view()
	err = c.saveLocked()
	c.mu.Unlock()
	c.log.Info("model download started", "model_id", id, "source", req.Source)
	c.changed()
	return m, err
}

// Update changes a model's name, tags or recommended classes.
func (c *Catalog) Update(id string, p Patch) (Model, error) {
	var tags, classes []string
	var err error
	if p.Tags != nil {
		if tags, err = normTags(p.Tags); err != nil {
			return Model{}, err
		}
	}
	if p.RecommendedClasses != nil {
		if classes, err = normClasses(p.RecommendedClasses); err != nil {
			return Model{}, err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.models[id]
	if !ok {
		return Model{}, ErrUnknownModel
	}
	if p.Name != nil {
		if strings.TrimSpace(*p.Name) == "" {
			return Model{}, invalid("name must not be empty")
		}
		e.m.Name = strings.TrimSpace(*p.Name)
	}
	if p.Tags != nil {
		e.m.Tags = tags
	}
	if p.RecommendedClasses != nil {
		e.m.RecommendedClasses = classes
	}
	return e.view(), c.saveLocked()
}

// Remove stops a download if one runs and deletes the model and its file.
func (c *Catalog) Remove(id string) error {
	c.mu.Lock()
	e, ok := c.models[id]
	if !ok {
		c.mu.Unlock()
		return ErrUnknownModel
	}
	if e.cancel != nil {
		e.cancel()
	}
	delete(c.models, id)
	err := c.saveLocked()
	dir := c.dir
	c.mu.Unlock()
	if dir != "" {
		// A running download notices the cancel and removes nothing
		// itself; the files go here. A node still reading the file keeps
		// its open descriptor.
		_ = os.Remove(filepath.Join(dir, id+".gguf"))
		_ = os.Remove(filepath.Join(dir, id+".gguf.part"))
	}
	c.log.Info("model removed", "model_id", id)
	c.changed()
	return err
}

// Close stops running downloads (they resume on the next start) and waits
// for them to return.
func (c *Catalog) Close() {
	c.mu.Lock()
	c.closing = true
	for _, e := range c.models {
		if e.cancel != nil {
			e.cancel()
		}
	}
	c.mu.Unlock()
	c.wg.Wait()
}

// saveLocked writes the catalog file. Call with mu held.
func (c *Catalog) saveLocked() error {
	if c.opts.StateFile == "" {
		return nil
	}
	f := catalogFile{Version: 1, Models: make([]Model, 0, len(c.models))}
	for _, e := range c.models {
		m := e.m
		m.Progress, m.Default, m.NodesServing = 0, false, 0
		f.Models = append(f.Models, m)
	}
	slices.SortFunc(f.Models, func(a, b Model) int { return cmp.Compare(a.ID, b.ID) })
	data, err := json.MarshalIndent(f, "", " ")
	if err != nil {
		return err
	}
	if err := gateway.WriteFileAtomic(c.opts.StateFile, data, 0o600); err != nil {
		c.log.Error("saving model catalog", "err", err)
		return err
	}
	return nil
}

func normTags(in []string) ([]string, error) {
	out := []string{}
	for _, t := range in {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || slices.Contains(out, t) {
			continue
		}
		if len(t) > 64 || strings.ContainsAny(t, ", \t\n") {
			return nil, invalid("tag %q: at most 64 characters, no commas or spaces", t)
		}
		out = append(out, t)
	}
	return out, nil
}

func normClasses(in []string) ([]string, error) {
	out := []string{}
	for _, c := range in {
		c = strings.ToLower(strings.TrimSpace(c))
		if c == "" || slices.Contains(out, c) {
			continue
		}
		if !IsClass(c) {
			return nil, invalid("unknown device class %q", c)
		}
		out = append(out, c)
	}
	// Keep class order (xs..xl) regardless of input order.
	slices.SortFunc(out, func(a, b string) int { return cmp.Compare(classIndex(a), classIndex(b)) })
	return out, nil
}

func classIndex(id string) int {
	return slices.IndexFunc(Classes, func(c Class) bool { return c.ID == id })
}
