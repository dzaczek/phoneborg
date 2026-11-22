package proto

import (
	"io"
	"sync"
)

// PathPrimaDial is the endpoint a prima.cpp worker's agent connects to
// (HTTP Upgrade, ADR-037): GET /v0/nodes/{id}/prima-dial?to=next|master. The
// controller answers 101 and splices the connection to the ring peer the
// worker would otherwise dial directly.
const PathPrimaDial = "/v0/nodes/{id}/prima-dial"

// UpgradeProtocol is the Upgrade header value of PathPrimaDial.
const UpgradeProtocol = "phoneborg-tunnel"

// Splice copies a to b and b to a until either side ends, then closes both.
func Splice(a, b io.ReadWriteCloser) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src io.ReadWriteCloser) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		_ = dst.Close() // unblocks the other direction
		_ = src.Close()
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}
