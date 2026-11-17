//go:build !linux

package membership

// monitorLoop is a no-op on non-Linux platforms
func (m *IPMonitor) monitorLoop(stop <-chan struct{}) {}

// periodicReconcile is a no-op on non-Linux platforms
func (m *IPMonitor) periodicReconcile(stop <-chan struct{}) {}

// enforceExpectations is a no-op on non-Linux platforms
func (m *IPMonitor) enforceExpectations() {}
