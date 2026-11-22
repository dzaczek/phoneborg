package nodeagent

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dzaczek/phoneborg/proto"
)

// prima.cpp workers (ADR-037). Rank r binds its data port (data_port + r)
// and sends to its next rank at data_port + r + 1 and to the master at
// data_port + 0, all on one IP. The agent gives every worker its own
// data_port so that the bind port is the serving port, which the existing
// adb forward already exposes to the host: data_port = servePort - rank. The
// sends then go to 127.0.0.1:servePort+1 (next) and 127.0.0.1:servePort-rank
// (master, and the next of the last rank), where the agent listens and
// tunnels to the controller.

// primaPorts returns a worker's prima.cpp data and signal ports. The signal
// port binds servePort+2, which nothing else uses; the signal is only sent
// at shutdown.
func primaPorts(servePort, rank int) (data, signal int) {
	return servePort - rank, servePort + 2 - rank
}

// primaWorkerArgs is the worker's llama-cli command line. -m is the only
// model: every rank needs the whole file, mmap pages in just its layers.
func primaWorkerArgs(modelPath string, servePort, threads int, r proto.PrimaRank) []string {
	data, signal := primaPorts(servePort, r.Rank)
	return []string{"-m", modelPath, "--world", strconv.Itoa(r.World), "--rank", strconv.Itoa(r.Rank),
		"--master", "127.0.0.1", "--next", "127.0.0.1", "--data-port", strconv.Itoa(data), "--signal-port", strconv.Itoa(signal),
		"-c", strconv.Itoa(r.CtxSize), "-t", strconv.Itoa(threads), "--prefetch"}
}

// primaTunnelPorts maps the local ports a worker of rank r sends to onto the
// controller's targets: "next" for every rank but the last, and "master" at
// servePort-r (the last rank sends its next, rank 0, there too).
func primaTunnelPorts(servePort int, r proto.PrimaRank) map[int]string {
	out := map[int]string{servePort - r.Rank: "master"}
	if r.Rank < r.World-1 {
		out[servePort+1] = "next"
	}
	return out
}

// primaTunnel listens on a worker's send ports and carries every connection
// to the controller.
type primaTunnel struct {
	ctx    context.Context
	cancel context.CancelFunc
	lns    []net.Listener
	wg     sync.WaitGroup
	dial   func(ctx context.Context, to string) (net.Conn, error)
}

func startPrimaTunnel(ctx context.Context, ports map[int]string, dial func(ctx context.Context, to string) (net.Conn, error)) (*primaTunnel, error) {
	t := &primaTunnel{dial: dial}
	t.ctx, t.cancel = context.WithCancel(ctx)
	for port, to := range ports {
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			t.Close()
			return nil, err
		}
		t.lns = append(t.lns, ln)
		t.wg.Add(1)
		go t.accept(ln, to)
	}
	return t, nil
}

func (t *primaTunnel) accept(ln net.Listener, to string) {
	defer t.wg.Done()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			up, err := t.dial(t.ctx, to)
			if err != nil {
				c.Close() // the worker's ZeroMQ socket reconnects
				return
			}
			stop := context.AfterFunc(t.ctx, func() { c.Close(); up.Close() })
			defer stop()
			proto.Splice(c, up)
		}()
	}
}

// Close stops listening and ends every tunnelled connection.
func (t *primaTunnel) Close() {
	t.cancel()
	for _, ln := range t.lns {
		ln.Close()
	}
	t.wg.Wait()
}

// dialController opens an upgraded connection to the controller's
// prima-dial endpoint for node id.
func dialController(ctx context.Context, controllerURL, id, to string) (net.Conn, error) {
	u, err := url.Parse(controllerURL)
	if err != nil || u.Scheme != "http" {
		return nil, fmt.Errorf("prima tunnel needs an http controller URL, got %q", controllerURL)
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	c, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		controllerURL+strings.Replace(proto.PathPrimaDial, "{id}", url.PathEscape(id), 1)+"?to="+to, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", proto.UpgradeProtocol)
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	if err := req.Write(c); err != nil {
		c.Close()
		return nil, err
	}
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		c.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		c.Close()
		return nil, fmt.Errorf("prima-dial %s: HTTP %d", to, resp.StatusCode)
	}
	_ = c.SetDeadline(time.Time{})
	return &bufConn{Conn: c, r: br}, nil
}

// bufConn reads through the bufio.Reader that parsed the upgrade response,
// which may hold the first tunnelled bytes.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }
