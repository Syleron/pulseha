package integration

import (
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/syleron/pulseha/tests/testutils"
)

func TestClusterFormation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("integration tests run only on Linux")
	}
	// Set test mode to skip hostname validation
	os.Setenv("PULSEHA_TEST", "true")
	defer os.Unsetenv("PULSEHA_TEST")

	cluster := testutils.NewTestCluster()
	defer cluster.Cleanup()

	// Add first node
	node1, err := cluster.AddNode("node1")
	require.NoError(t, err, "Failed to add first node")

	// Start first node
	err = node1.Start()
	require.NoError(t, err, "Failed to start first node")

	// Wait for first node to be ready
	time.Sleep(500 * time.Millisecond)

	// Add second node
	node2, err := cluster.AddNode("node2")
	require.NoError(t, err, "Failed to add second node")

	// Start second node
	err = node2.Start()
	require.NoError(t, err, "Failed to start second node")

	// Wait for second node to be ready
	time.Sleep(500 * time.Millisecond)

	// Join second node to first node
	err = node2.Join(node1)
	require.NoError(t, err, "Failed to join second node to cluster")

	// Wait for cluster to stabilize
	time.Sleep(1 * time.Second)

	// Verify node1's config
	require.NotNil(t, node1.Config, "Node1 config should not be nil")
	require.Equal(t, 2, len(node1.Config.Nodes), "Node1 should have 2 nodes in config")
	require.Contains(t, node1.Config.Nodes, node2.ID, "Node2 should be in Node1's config")

	// Verify node2's config
	require.NotNil(t, node2.Config, "Node2 config should not be nil")
	require.Equal(t, 2, len(node2.Config.Nodes), "Node2 should have 2 nodes in config")
	require.Contains(t, node2.Config.Nodes, node1.ID, "Node1 should be in Node2's config")

	// Verify node statuses
	node1Status := node1.GetMemberStatus(node2.Hostname)
	require.Equal(t, "passive", node1Status, "Node2 should be passive in Node1's view")

	node2Status := node2.GetMemberStatus(node1.Hostname)
	require.Equal(t, "active", node2Status, "Node1 should be active in Node2's view")
}

func TestClusterHealthCheck(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("integration tests run only on Linux")
	}
	// Create a new test cluster
	cluster := testutils.NewTestCluster()
	defer cluster.Cleanup()

	// Add and start first node
	node1, err := cluster.AddNode("node1")
	require.NoError(t, err, "Failed to add first node")
	err = node1.Start()
	require.NoError(t, err, "Failed to start first node")

	// Wait for first node to be ready
	time.Sleep(500 * time.Millisecond)

	// Add and start second node
	node2, err := cluster.AddNode("node2")
	require.NoError(t, err, "Failed to add second node")
	err = node2.Start()
	require.NoError(t, err, "Failed to start second node")

	// Wait for second node to be ready
	time.Sleep(500 * time.Millisecond)

	// Join node2 to the cluster
	err = node2.Join(node1)
	require.NoError(t, err, "Failed to join second node to cluster")

	// Wait for health checks to run
	time.Sleep(1 * time.Second)

	// Verify both nodes are healthy
	node1Status := node1.GetMemberStatus(node2.Hostname)
	require.Equal(t, "passive", node1Status, "Node2 should be passive in Node1's view")

	node2Status := node2.GetMemberStatus(node1.Hostname)
	require.Equal(t, "active", node2Status, "Node1 should be active in Node2's view")
}

func TestActiveActiveMode(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("integration tests run only on Linux")
	}
	// Set test mode to skip hostname validation
	os.Setenv("PULSEHA_TEST", "true")
	defer os.Unsetenv("PULSEHA_TEST")

	cluster := testutils.NewTestCluster()
	defer cluster.Cleanup()

	// Add first node
	node1, err := cluster.AddNode("node1")
	require.NoError(t, err, "Failed to add first node")

	// Set active-active mode in the configuration
	node1.Config.Pulse.Mode = "active-active"

	// Start first node
	err = node1.Start()
	require.NoError(t, err, "Failed to start first node")

	// Wait for first node to be ready
	time.Sleep(500 * time.Millisecond)

	// Add second node
	node2, err := cluster.AddNode("node2")
	require.NoError(t, err, "Failed to add second node")

	// Set active-active mode in the configuration
	node2.Config.Pulse.Mode = "active-active"

	// Start second node
	err = node2.Start()
	require.NoError(t, err, "Failed to start second node")

	// Wait for second node to be ready
	time.Sleep(500 * time.Millisecond)

	// Join second node to first node
	err = node2.Join(node1)
	require.NoError(t, err, "Failed to join second node to cluster")

	// Wait for cluster to stabilize
	time.Sleep(1 * time.Second)

	// Verify both nodes are configured for active-active mode
	require.Equal(t, "active-active", node1.Config.Pulse.Mode, "Node1 should be in active-active mode")
	require.Equal(t, "active-active", node2.Config.Pulse.Mode, "Node2 should be in active-active mode")

	// Verify both nodes recognize each other in their configurations
	require.Len(t, node1.Config.Nodes, 2, "Node1 should have 2 nodes in its configuration")
	require.Len(t, node2.Config.Nodes, 2, "Node2 should have 2 nodes in its configuration")

	// Create a group on node1
	err = node1.CreateGroup("group1")
	require.NoError(t, err, "Failed to create group on node1")

	// Add IPs to the group
	err = node1.AddIPsToGroup("group1", []string{"10.0.0.1", "10.0.0.2"})
	require.NoError(t, err, "Failed to add IPs to group")

	// Wait for the configuration to propagate
	time.Sleep(500 * time.Millisecond)

	// Assign the group to node1's interface
	err = node1.AssignGroupToInterface("group1", "eth0")
	require.NoError(t, err, "Failed to assign group to node1")

	// Assign the group to node2's interface
	err = node2.AssignGroupToInterface("group1", "eth0")
	require.NoError(t, err, "Failed to assign group to node2")

	// Wait for health checks to run
	time.Sleep(1 * time.Second)

	// Check the statuses of both nodes
	node1Status := node1.GetMemberStatus(node1.Hostname)
	node2Status := node2.GetMemberStatus(node2.Hostname)

	// In active-active mode, all eligible nodes should be active
	require.Equal(t, "active", node1Status,
		"Node1 should be active in active-active mode")
	require.Equal(t, "active", node2Status,
		"Node2 should be active in active-active mode")

	// Log the active IPs from both nodes
	t.Logf("Node1 active IPs: %v", node1.GetActiveIPs())
	t.Logf("Node2 active IPs: %v", node2.GetActiveIPs())

	// Verify that at least one node has active IPs
	allActiveIPs := append(node1.GetActiveIPs(), node2.GetActiveIPs()...)
	require.NotEmpty(t, allActiveIPs, "At least one node should have active IPs")
}
