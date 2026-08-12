//go:build !linux

package membership

// monitorLoop is a no-op on non-Linux platforms
func (m *IPMonitor) monitorLoop() {}

// periodicReconcile is a no-op on non-Linux platforms
func (m *IPMonitor) periodicReconcile() {}

// enforceExpectations is a no-op on non-Linux platforms
func (m *IPMonitor) enforceExpectations() {}
