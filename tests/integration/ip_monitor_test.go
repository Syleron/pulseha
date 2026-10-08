package integration

import (
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/syleron/pulseha/tests/integration/testutil"
	"github.com/syleron/pulseha/tests/testutils"
)

// TestIPMonitoring tests the IP monitoring system
func TestIPMonitoring(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("integration tests run only on Linux")
	}
	// Skip if not running as root (needed for IP manipulation)
	if !testutil.HasNetAdmin() {
		t.Skip("This test requires CAP_NET_ADMIN to manage interface addresses")
	}

	// Create a new test cluster
	cluster := testutils.NewTestCluster()
	defer cluster.Cleanup()

	// Add and start first node
	node1, err := cluster.AddNode("node1")
	require.NoError(t, err, "Failed to add first node")
	err = node1.Start()
	require.NoError(t, err, "Failed to start first node")

	// Wait for node to be ready
	time.Sleep(500 * time.Millisecond)

	// Add and start second node
	node2, err := cluster.AddNode("node2")
	require.NoError(t, err, "Failed to add second node")
	err = node2.Start()
	require.NoError(t, err, "Failed to start second node")

	// Join second node to first node
	err = node2.Join(node1)
	require.NoError(t, err, "Failed to join second node to cluster")

	// Wait for cluster to stabilize
	time.Sleep(1 * time.Second)

	// Create a test group
	groupName := "test-group"
	err = node1.CreateGroup(groupName)
	require.NoError(t, err, "Failed to create group")

	// Add test IP to the group
	testIP := "192.168.100.100"
	err = node1.AddIPsToGroup(groupName, []string{testIP})
	require.NoError(t, err, "Failed to add IP to group")

	// Assign group to node1's loopback interface
	err = node1.AssignGroupToInterface(groupName, "lo")
	require.NoError(t, err, "Failed to assign group to interface")
	err = node2.AssignGroupToInterface(groupName, "lo")
	require.NoError(t, err, "Failed to assign group to interface")

	// Promote node1 to make it active
	err = node2.PromoteNode(node1.Hostname, []string{testIP})
	require.NoError(t, err, "Failed to promote node1")

	// Wait for IP to be configured
	time.Sleep(2 * time.Second)

	// Verify IP is active on node1
	activeIPs := node1.GetActiveIPs()
	require.Contains(t, activeIPs, testIP, "IP should be active on node1")

	// Verify IP is actually on the interface
	hasIP, err := testutil.HasIPOnInterface("lo", testIP)
	require.NoError(t, err, "Failed to check if IP is on interface")
	require.True(t, hasIP, "IP should be on the loopback interface")

	// Now manually remove the IP
	t.Log("Manually removing IP from interface")
	cmd := exec.Command("ip", "addr", "del", testIP+"/32", "dev", "lo")
	err = cmd.Run()
	require.NoError(t, err, "Failed to remove IP")

	// Wait for IP monitor to detect and restore the IP
	t.Log("Waiting for IP monitor to restore the IP")
	time.Sleep(5 * time.Second)

	// Verify IP has been restored
	hasIP, err = testutil.HasIPOnInterface("lo", testIP)
	require.NoError(t, err, "Failed to check if IP is on interface")
	require.True(t, hasIP, "IP should have been restored by the IP monitor")

	// Now test failover
	t.Log("Testing failover")

	// Stop node1 to simulate failure
	err = cluster.StopNode(node1.Hostname)
	require.NoError(t, err, "Failed to stop node1")

	// Wait for failover to occur
	time.Sleep(5 * time.Second)

	// Verify IP is now active on node2
	activeIPs = node2.GetActiveIPs()
	require.Contains(t, activeIPs, testIP, "IP should be active on node2 after failover")

	// Verify IP is actually on node2's interface
	hasIP, err = testutil.HasIPOnInterface("lo", testIP)
	require.NoError(t, err, "Failed to check if IP is on interface")
	require.True(t, hasIP, "IP should be on node2's loopback interface after failover")
}
