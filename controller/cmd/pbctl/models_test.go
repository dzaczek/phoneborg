package main

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dzaczek/phoneborg/controller"
	"github.com/dzaczek/phoneborg/controller/gateway"
	"github.com/dzaczek/phoneborg/controller/models"
	"github.com/dzaczek/phoneborg/controller/models/modeltest"
	"github.com/dzaczek/phoneborg/proto"
)

func TestModelAndPlacementCommands(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cat, err := models.NewCatalog(models.CatalogOptions{Dir: filepath.Join(dir, "models")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cat.Close)
	reg := controller.NewRegistry(time.Hour, time.Hour, log)
	srv := controller.NewServer(reg, time.Second, controller.GatewayOptions{
		Models: controller.ModelOptions{Catalog: cat},
		Config: gateway.Config{UpstreamTimeout: time.Second, MaxAttempts: 1, Cooldown: time.Second}},
		controller.AdminOptions{Token: token}, log)
	for id, ram := range map[string]uint64{"n1": 4 << 30, "n2": 8 << 30} {
		reg.Register(proto.RegisterRequest{NodeID: id, Inventory: proto.Inventory{RAMTotalBytes: ram}}, "x")
	}
	srv.Replan()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	env := map[string]string{"PHONEBORG_URL": ts.URL, "PHONEBORG_ADMIN_TOKEN": token}
	src := filepath.Join(dir, "Gemma-3-1B-IT-Q4_0.gguf")
	os.WriteFile(src, modeltest.GGUF(modeltest.LlamaLike("gemma3", 26, 4, 1, 1152, 32768), 0), 0o600)
	const id = "gemma-3-1b-it-q4_0"

	run := func(args ...string) string {
		t.Helper()
		stdout, stderr, code := pbctl(t, env, args...)
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, stderr)
		}
		return stdout
	}
	expect := func(out string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Errorf("output lacks %q:\n%s", w, out)
			}
		}
	}

	expect(run("models"), "no models in the catalog")
	expect(run("models", "add", "file://"+src, "-tag", "chat,small", "-recommend", "s"), "model "+id+" is downloading")
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if m, _ := cat.Get(id); m.Status == models.StatusReady {
			break
		}
	}
	expect(run("models"), "MODEL", "FITS (16k)", id, "1.5B", "Q4_K_M", "ready", "chat,small", "s,m,l,xl")
	expect(run("models", "tag", id, "coding"), "tagged coding")
	expect(run("models", "recommend", id, "m,l"), "recommended")                        // old positional form
	expect(run("models", "recommend", id, "classes=s,m", "tiers=t2,t3"), "recommended") // new key=value form
	expect(run("classes"), "CLASS", "xs", "10 GiB+", id, "TIER", "t1", "25 GB/s+")
	expect(run("nodes"), "CLASS", "n1", "s/?", "n2", "l/?") // no self-test yet: tier "?"
	expect(run("models", "default", id), "default model is "+id)
	expect(run("models"), id+" (default)", "coding")
	expect(run("placement"), "default model: "+id, "NODE", "n1", "default", "PRED TOK/S")
	expect(run("placement", "preview", id, "replicas=1", "classes=l"), "preview, nothing applied", "replicas")
	expect(run("placement"), "no policies")
	expect(run("placement", "set", id, "pin=n1"), "pin", "n1")
	expect(run("placement", "set", id, "percent=50", "classes=s,l", "min_tok_s=3"), "percent", "50%", "s,l", "MIN TOK/S", "3")
	expect(run("placement", "-json"), `"mode": "percent"`, `"min_tok_s": 3`)
	expect(run("placement", "unset", id), "no policies")
	expect(run("models", "default", "none"), "no default model")
	expect(run("models", "rm", id), "model "+id+" removed")

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"placement", "set", id, "replicas=2"}, "unknown model"},
		{[]string{"placement", "set", "x", "replicas=2", "pin=n1"}, "choose one of"},
		{[]string{"placement", "set", "x", "classes=s"}, "want pin="},
		{[]string{"placement", "unset", "x"}, "no policy for model x"},
		{[]string{"models", "add", "ftp://x/y.gguf"}, "scheme must be"},
		{[]string{"models", "add", "https://x/y.gguf", "-color", "red"}, "unknown option"},
		{[]string{"models", "rm", "x"}, "HTTP 404"},
	} {
		if _, stderr, code := pbctl(t, env, tc.args...); code != 1 || !strings.Contains(stderr, tc.want) {
			t.Errorf("%v: exit %d, stderr %q; want %q", tc.args, code, stderr, tc.want)
		}
	}
}
