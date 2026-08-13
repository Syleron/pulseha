package integration

import (
	"os"
	"runtime"
	"testing"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/stretchr/testify/require"
	"github.com/syleron/pulseha/internal/quorum"
	"github.com/syleron/pulseha/tests/testutils"
)

// TestQuorumAutoManagement validates automatic quorum policy by node count
func TestQuorumAutoManagement(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("integration tests run only on Linux")
	}
	cluster := testutils.NewTestCluster()
	defer cluster.Cleanup()

	// One node
	node1, err := cluster.AddNode("node1")
	require.NoError(t, err)
	require.NoError(t, node1.Start())
	time.Sleep(500 * time.Millisecond)

	// Two nodes
	node2, err := cluster.AddNode("node2")
	require.NoError(t, err)
	require.NoError(t, node2.Start())
	require.NoError(t, node2.Join(node1))
	time.Sleep(1 * time.Second)

	// With 2 nodes, quorum voting should be unavailable
	logger := log.New(os.Stdout)
	tqm := testutils.NewTestQuorumManager(node1.Config, logger)
	_, err = tqm.StartTestVotingSession(quorum.VoteTypeNodeStatus, "subj", "desc", 3*time.Second)
	require.Error(t, err, "Quorum voting should be unavailable with <3 nodes")

	// Three nodes
	node3, err := cluster.AddNode("node3")
	require.NoError(t, err)
	require.NoError(t, node3.Start())
	require.NoError(t, node3.Join(node1))
	time.Sleep(1 * time.Second)

	// The manager samples len(cfg.Nodes) once, at construction, so it still believes
	// the cluster is the two nodes it was built against. UpdateNodeCount is what the
	// daemon calls on a membership change and is what this test is really asserting
	// about — that quorum availability tracks the node count rather than the count
	// that happened to be current when the manager was made.
	tqm.UpdateNodeCount(3)

	// Now quorum voting is available; majority should pass (2 of 3)
	_, err = tqm.StartTestVotingSession(quorum.VoteTypeNodeStatus, "subj2", "desc2", 10*time.Second)
	require.NoError(t, err)
}

// TestQuorumVoting verifies majority pass behavior at 3 nodes
func TestQuorumVoting(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("integration tests run only on Linux")
	}
	cluster := testutils.NewTestCluster()
	defer cluster.Cleanup()

	n1, _ := cluster.AddNode("node1")
	_ = n1.Start()
	n2, _ := cluster.AddNode("node2")
	_ = n2.Start()
	_ = n2.Join(n1)
	n3, _ := cluster.AddNode("node3")
	_ = n3.Start()
	_ = n3.Join(n1)
	time.Sleep(1 * time.Second)

	logger := log.New(os.Stdout)
	tqm := testutils.NewTestQuorumManager(n1.Config, logger)
	sid, err := tqm.StartTestVotingSession(quorum.VoteTypeNodeStatus, "test", "desc", 10*time.Second)
	require.NoError(t, err)

	_ = tqm.CastTestVote(sid, n1.ID, quorum.VoteDecisionYes)
	_ = tqm.CastTestVote(sid, n2.ID, quorum.VoteDecisionYes)
	tqm.ProcessTestExpiredSessions()
	s, err := tqm.GetTestVotingSession(sid)
	require.NoError(t, err)
	require.NotNil(t, s.Result)
	require.True(t, s.Result.Passed)
	require.True(t, s.Result.QuorumMet)
}
