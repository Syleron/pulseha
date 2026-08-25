package integration

import (
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/syleron/pulseha/rpc"
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
	requireMemberStatus(t, node1, node2.Hostname, "passive", "Node2 should be passive in Node1's view")
	requireMemberStatus(t, node2, node1.Hostname, "active", "Node1 should be active in Node2's view")
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
	requireMemberStatus(t, node1, node2.Hostname, "passive", "Node2 should be passive in Node1's view")
	requireMemberStatus(t, node2, node1.Hostname, "active", "Node1 should be active in Node2's view")
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
	// In active-active mode, all eligible nodes should be active
	requireMemberStatus(t, node1, node1.Hostname, "active", "Node1 should be active in active-active mode")
	requireMemberStatus(t, node2, node2.Hostname, "active",
		"Node2 should be active in active-active mode")

	// Log the active IPs from both nodes
	t.Logf("Node1 active IPs: %v", node1.GetActiveIPs())
	t.Logf("Node2 active IPs: %v", node2.GetActiveIPs())

	// Verify that at least one node has active IPs
	allActiveIPs := append(node1.GetActiveIPs(), node2.GetActiveIPs()...)
	require.NotEmpty(t, allActiveIPs, "At least one node should have active IPs")
}

// END-2289. Two nodes in one healthy cluster published contradictory views of
// that cluster at the same instant: asked on the elected node, its own row read
// Standby; asked on its peer, the same node read Active. Nothing was wrong with
// the cluster — the elected node held no addresses because none were configured,
// and it was the only observer permitted to draw a conclusion from that.
//
// On the appliance the two views became "Standby / No Data" with cluster health
// 1·1 on one node against "Active / Online" and 2·0 on the other.
//
// The generic agreement check at the end is the invariant, and it is deliberately
// not spelled out as a pair of expected values: whatever a node publishes about
// a member, every other node must publish the same thing, including values this
// test was never taught to expect.
func TestNodesAgreeOnTheStatusTheyPublish(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("integration tests run only on Linux")
	}
	os.Setenv("PULSEHA_TEST", "true")
	defer os.Unsetenv("PULSEHA_TEST")

	cluster := testutils.NewTestCluster()
	defer cluster.Cleanup()

	node1, err := cluster.AddNode("node1")
	require.NoError(t, err, "Failed to add first node")
	// Set explicitly rather than relying on a default: the harness writes no mode
	// at all, and an empty mode only behaves like active-passive by accident of
	// every gate testing for the other value. The appliance ships active-passive
	// (packages/config/config.go), and that is the configuration under test.
	node1.Config.Pulse.Mode = "active-passive"
	require.NoError(t, node1.Start(), "Failed to start first node")
	require.NoError(t, cluster.WaitForPort("node1", 40), "node1 never accepted connections")

	node2, err := cluster.AddNode("node2")
	require.NoError(t, err, "Failed to add second node")
	node2.Config.Pulse.Mode = "active-passive"
	require.NoError(t, node2.Start(), "Failed to start second node")
	require.NoError(t, cluster.WaitForPort("node2", 40), "node2 never accepted connections")

	require.NoError(t, node2.Join(node1), "Failed to join second node to cluster")

	// No groups are created anywhere in this test on purpose. A freshly paired
	// appliance has no floating IPs, which is what made the elected node's
	// assignment list empty and put it one branch away from Standby.
	//
	// Asked of the daemon rather than read off the config struct. The struct is
	// what this test requested; only the daemon can say what it is acting on, and
	// asserting the request would make the precondition self-referential.
	for _, n := range []*testutils.TestNode{node1, node2} {
		mode, err := n.ReportedMode()
		require.NoError(t, err, "%s should report its cluster mode", n.Hostname)
		require.Equal(t, "active-passive", mode, "%s must be running the mode under test", n.Hostname)
	}

	// The defect, from the affected node's own point of view: elected, holding
	// nothing, and nothing to hold. Active is the answer.
	requireReportedStatus(t, node1, node1.Hostname, rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE,
		"the elected node must not call itself Standby in active-passive")
	requireReportedStatus(t, node2, node1.Hostname, rpc.MemberStatusEnum_MEMBER_STATUS_ACTIVE,
		"the peer's view of the elected node was always correct and must stay so")
	requireReportedStatus(t, node1, node2.Hostname, rpc.MemberStatusEnum_MEMBER_STATUS_PASSIVE,
		"the passive node's row was always correct and must stay so")
	requireReportedStatus(t, node2, node2.Hostname, rpc.MemberStatusEnum_MEMBER_STATUS_PASSIVE,
		"the passive node must agree with its peer about itself")

	for _, target := range []string{node1.Hostname, node2.Hostname} {
		requireAgreedStatus(t, node1, node2, target,
			"both nodes must publish the same status for "+target)
	}
}
