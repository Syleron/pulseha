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

// TestCombinedFeatures tests both quorum voting and IP monitoring together
func TestCombinedFeatures(t *testing.T) {
	// Skip platform and root requirements
	if runtime.GOOS != "linux" {
		t.Skip("integration tests run only on Linux")
	}
	if !testutil.HasNetAdmin() {
		t.Skip("This test requires CAP_NET_ADMIN to manage interface addresses")
	}

	// Create a new test cluster
	cluster := testutils.NewTestCluster()
	defer cluster.Cleanup()

	// Add and start three nodes
	node1, err := cluster.AddNode("node1")
	require.NoError(t, err, "Failed to add first node")
	err = node1.Start()
	require.NoError(t, err, "Failed to start first node")

	node2, err := cluster.AddNode("node2")
	require.NoError(t, err, "Failed to add second node")
	err = node2.Start()
	require.NoError(t, err, "Failed to start second node")

	node3, err := cluster.AddNode("node3")
	require.NoError(t, err, "Failed to add third node")
	err = node3.Start()
	require.NoError(t, err, "Failed to start third node")

	// Join nodes to form a cluster
	err = node2.Join(node1)
	require.NoError(t, err, "Failed to join second node to cluster")
	err = node3.Join(node1)
	require.NoError(t, err, "Failed to join third node to cluster")

	// Wait for cluster to stabilize
	time.Sleep(1 * time.Second)

	// Create a test group
	groupName := "test-group"
	err = node1.CreateGroup(groupName)
	require.NoError(t, err, "Failed to create group")

	// Add test IP to the group
	testIP := "192.168.100.200"
	err = node1.AddIPsToGroup(groupName, []string{testIP})
	require.NoError(t, err, "Failed to add IP to group")

	// Assign group to node1's loopback interface
	err = node1.AssignGroupToInterface(groupName, "lo")
	require.NoError(t, err, "Failed to assign group to interface")
	err = node2.AssignGroupToInterface(groupName, "lo")
	require.NoError(t, err, "Failed to assign group to interface")
	err = node3.AssignGroupToInterface(groupName, "lo")
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

	// Now manually remove the IP using ip command
	t.Log("Manually removing IP from interface")
	cmd := exec.Command("ip", "addr", "del", testIP+"/32", "dev", "lo")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "Failed to remove IP: %s", string(output))

	// Wait for IP monitor to detect and restore the IP
	t.Log("Waiting for IP monitor to restore the IP")
	time.Sleep(5 * time.Second)

	// Verify IP has been restored
	hasIP, err = testutil.HasIPOnInterface("lo", testIP)
	require.NoError(t, err, "Failed to check if IP is on interface")
	require.True(t, hasIP, "IP should have been restored by the IP monitor")

	// Test failover
	t.Log("Testing failover with automatic quorum policy in effect")

	// Stop node1 to simulate failure
	err = cluster.StopNode(node1.Hostname)
	require.NoError(t, err, "Failed to stop node1")

	// Wait for failover to occur (with quorum voting, this may take longer)
	time.Sleep(10 * time.Second)

	// Verify IP is now active on one of the remaining nodes
	node2ActiveIPs := node2.GetActiveIPs()
	node3ActiveIPs := node3.GetActiveIPs()

	ipFound := false
	if testutil.Contains(node2ActiveIPs, testIP) {
		ipFound = true
		t.Log("IP failed over to node2")
	} else if testutil.Contains(node3ActiveIPs, testIP) {
		ipFound = true
		t.Log("IP failed over to node3")
	}

	require.True(t, ipFound, "IP should have failed over to one of the remaining nodes")

	// Verify IP is actually on the interface of one of the remaining nodes
	hasIP, err = testutil.HasIPOnInterface("lo", testIP)
	require.NoError(t, err, "Failed to check if IP is on interface")
	require.True(t, hasIP, "IP should be on the loopback interface of one of the remaining nodes")

	// Now test what happens when we remove a node and we are left with 2 nodes
	t.Log("Testing behavior with 2 nodes (no quorum policy)")

	// Stop node3 to leave only 2 nodes
	err = cluster.StopNode(node3.Hostname)
	require.NoError(t, err, "Failed to stop node3")

	// Wait for cluster to stabilize
	time.Sleep(5 * time.Second)

	// Manually remove the IP again
	t.Log("Manually removing IP from interface again")
	cmd = exec.Command("ip", "addr", "del", testIP+"/32", "dev", "lo")
	cmd.Run() // Ignore errors as the IP might be on a different node

	// Wait for IP monitor to detect and restore the IP
	t.Log("Waiting for IP monitor to restore the IP")
	time.Sleep(5 * time.Second)

	// Verify IP has been restored
	hasIP, err = testutil.HasIPOnInterface("lo", testIP)
	require.NoError(t, err, "Failed to check if IP is on interface")
	require.True(t, hasIP, "IP should have been restored by the IP monitor")
}
