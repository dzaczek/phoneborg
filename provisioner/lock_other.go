//go:build !linux && !darwin

package provisioner

// Lock is a no-op on platforms without flock (ADR-019 targets Linux/macOS
// adb hosts); pcprov here relies on the operator not running two instances.
func Lock(path string) (release func(), err error) {
	return func() {}, nil
}
