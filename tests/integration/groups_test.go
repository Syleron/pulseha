package integration

import (
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/syleron/pulseha/tests/integration/testutil"
	"github.com/syleron/pulseha/tests/testutils"
)

// TestGroupManagement tests the group management functionality
func TestGroupManagement(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("integration tests run only on Linux")
	}
	// Set environment variable to skip hostname validation
	os.Setenv("PULSEHA_TEST", "true")

	// Create a test cluster with 2 nodes
	cluster := testutils.NewTestCluster()
	defer cluster.Cleanup()

	// Add nodes to the cluster
	node1, err := cluster.AddNode("node1")
	require.NoError(t, err, "Failed to add node1")

	node2, err := cluster.AddNode("node2")
	require.NoError(t, err, "Failed to add node2")

	// Start the nodes
	err = node1.Start()
	require.NoError(t, err, "Failed to start node1")

	err = node2.Start()
	require.NoError(t, err, "Failed to start node2")

	// Join node2 to node1
	err = node2.Join(node1)
	require.NoError(t, err, "Failed to join node2 to node1")

	// Create a test group
	groupName := "group1"
	ips := []string{"192.168.1.10", "192.168.1.11"}
	// What the daemon stores. AddIPToGroup canonicalises a bare address to CIDR,
	// which the old harness never saw because it wrote the raw strings into its own
	// config struct instead of going through the RPC.
	storedIPs := []string{"192.168.1.10/32", "192.168.1.11/32"}

	// Add the group to node1
	err = node1.CreateGroup(groupName)
	require.NoError(t, err, "Failed to create group")

	// Add IPs to the group
	err = node1.AddIPsToGroup(groupName, ips)
	require.NoError(t, err, "Failed to add IPs to group")

	// Assign the group to node1's interface
	err = node1.AssignGroupToInterface(groupName, "eth0")
	require.NoError(t, err, "Failed to assign group to interface")

	// Verify the group exists
	group, err := node1.GetGroup(groupName)
	require.NoError(t, err, "Failed to get group")
	require.Equal(t, storedIPs, group, "Group IPs don't match")

	// Manually test the GetActiveIPs functionality
	t.Log("Testing GetActiveIPs functionality directly")

	// Set node1 to active and check its active IPs
	node1.SetStatus(testutils.StatusActive)
	node1ActiveIPs := node1.GetActiveIPs()
	t.Logf("Node1 active IPs: %v", node1ActiveIPs)

	// Verify node1 has the expected IPs
	for _, ip := range storedIPs {
		if !contains(node1ActiveIPs, ip) {
			t.Errorf("Expected node1 to have IP %s, but got active IPs: %v", ip, node1ActiveIPs)
		}
	}

	// Fail over to node2, through the daemon rather than by flipping a field.
	//
	// This used to be two SetStatus calls, which write member.Status on the member
	// list directly. Nothing moves an address as a result -- the promotion path
	// does -- so node2 was asserted to hold addresses that nothing had ever given
	// it, and the assertion had simply never run: ./tests/... was not built by any
	// CI step until defect #74.
	//
	// node2 also has to be able to host the group at all. AssignGroupToInterface
	// was only ever called on node1, so even a real promotion had nowhere to put
	// the addresses on node2.
	// Wait for the group to reach node2's daemon first. It was created through
	// node1's daemon, and propagation is asynchronous -- the broadcaster coalesces
	// on a 250ms linger and retries beyond that -- so assigning immediately raced
	// it and node2 answered "group group1 does not exist".
	groupDeadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(groupDeadline) {
		if g, gErr := node2.GetGroup(groupName); gErr == nil && len(g) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	err = node2.AssignGroupToInterface(groupName, "eth0")
	require.NoError(t, err, "Failed to assign group to node2's interface")

	t.Log("Failing over to node2 via the promotion RPC")
	err = node2.PromoteNode(node2.Hostname, storedIPs)
	require.NoError(t, err, "Failed to promote node2")

	// Poll rather than sleep: promotion is asynchronous on the daemon side, so a
	// fixed wait asserts at one arbitrary instant and lets a loaded runner decide
	// the result.
	var node2ActiveIPs []string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		node2ActiveIPs = node2.GetActiveIPs()
		missing := false
		for _, ip := range storedIPs {
			if !contains(node2ActiveIPs, ip) {
				missing = true
				break
			}
		}
		if !missing {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("Node2 active IPs: %v", node2ActiveIPs)

	// Verify node2 has taken over the IPs
	for _, ip := range storedIPs {
		if !contains(node2ActiveIPs, ip) {
			t.Errorf("Expected node2 to have IP %s after failover, but got active IPs: %v", ip, node2ActiveIPs)
		}
	}

	// Clean up
	cluster.StopNode(node1.Hostname)
	cluster.StopNode(node2.Hostname)
}

// TestGroupIPRemoval tests removing IPs from a group
func TestGroupIPRemoval(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("integration tests run only on Linux")
	}
	// Skip if not running as root (needed for IP manipulation)
	if !testutil.IsRoot() {
		t.Skip("This test requires root privileges to run")
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

	// Create a test group
	groupName := "test-group"
	err = node1.CreateGroup(groupName)
	require.NoError(t, err, "Failed to create group")

	// Add test IPs to the group
	ips := []string{"192.168.1.10", "192.168.1.11"}
	// What the daemon stores. AddIPToGroup canonicalises a bare address to CIDR,
	// which the old harness never saw because it wrote the raw strings into its own
	// config struct instead of going through the RPC.
	storedIPs := []string{"192.168.1.10/32", "192.168.1.11/32"}
	err = node1.AddIPsToGroup(groupName, ips)
	require.NoError(t, err, "Failed to add IPs to group")

	// Verify IPs are in the group
	group, err := node1.GetGroup(groupName)
	require.NoError(t, err, "Failed to get group")
	require.ElementsMatch(t, group, storedIPs, "Group should contain the added IPs")

	// Remove one IP from the group
	err = node1.RemoveIPFromGroup(groupName, ips[0])
	require.NoError(t, err, "Failed to remove IP from group")

	// Verify IP was removed
	group, err = node1.GetGroup(groupName)
	require.NoError(t, err, "Failed to get group")
	require.NotContains(t, group, storedIPs[0], "Group should not contain the removed IP")
	require.Contains(t, group, ips[1], "Group should still contain the other IP")
}

// Helper function to check if a slice contains a string
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
