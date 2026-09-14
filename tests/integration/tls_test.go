// PulseHA - HA Cluster Daemon
// Copyright (C) 2017-2021  Andrew Zak <andrew@linux.com>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package integration

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
	"github.com/syleron/pulseha/tests/testutils"
)

// A two-node cluster taken from plaintext to TLS while it is running, which is
// the one thing about #111 that no unit test can reach.
//
// The unit tests cover each piece — the handshake, the trust set, the delivery
// ordering, the listener rebind, the connection caches — and every one of them
// passed while at least three separate defects would still have taken a live
// cluster off the air. What they cannot do is put two real daemons on two real
// sockets and change the terms underneath them. This can, and it is the closest
// thing to the appliance pair that runs in CI.
//
// It is not a substitute for the pair: one machine, no network, no failover under
// load. docs/RUNBOOK-111-verify.md is still the verification that counts.
func TestTheClusterCanBeFlippedToTLSWhileItIsRunning(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("integration tests run only on Linux")
	}
	os.Setenv("PULSEHA_TEST", "true")
	defer os.Unsetenv("PULSEHA_TEST")

	cluster := testutils.NewTestCluster()
	defer cluster.Cleanup()

	node1, err := cluster.AddNode("node1")
	require.NoError(t, err)
	node2, err := cluster.AddNode("node2")
	require.NoError(t, err)

	// Each node gets its own identity, which is the whole reason the certificate
	// directory is a per-node value: these are two Servers in one process, and a
	// shared directory would give them one certificate between them.
	certs := t.TempDir()
	cert1, err := node1.GiveNodeAnIdentity(filepath.Join(certs, "node1"))
	require.NoError(t, err)
	cert2, err := node2.GiveNodeAnIdentity(filepath.Join(certs, "node2"))
	require.NoError(t, err)
	require.NotEqual(t, cert1, cert2, "the two nodes must not share an identity")

	require.NoError(t, node1.Start())
	time.Sleep(500 * time.Millisecond)
	require.NoError(t, node2.Start())
	time.Sleep(500 * time.Millisecond)
	require.NoError(t, node2.Join(node1))

	// Settled before the flip, and waited for rather than slept at. The flip's
	// precondition is that every node is reachable, so a test that flips before
	// the cluster has converged fails on the precondition and tells you nothing
	// about the flip -- which is exactly what a fixed sleep here produced.
	requireMemberStatus(t, node1, node2.Hostname, "passive", "node2 never settled")
	requireMemberStatus(t, node2, node1.Hostname, "active", "node1 never settled")

	// The permissive phase: both certificates published, nothing depending on
	// them yet. Published by hand here because PULSEHA_TEST skips the startup
	// generation these nodes would otherwise publish from.
	publish := func(n *testutils.TestNode, id, cert string) {
		cfg := n.Server.GetMemberList().Config()
		require.NotNil(t, cfg)
		if node, ok := cfg.Nodes[id]; ok && node != nil {
			node.TLSCert = cert
		}
	}
	for _, n := range []*testutils.TestNode{node1, node2} {
		publish(n, node1.ID, cert1)
		publish(n, node2.ID, cert2)
	}

	// The flip, run the way an operator runs it.
	var resp *rpc.UpdateConfigResponse
	require.Eventually(t, func() bool {
		var err error
		resp, err = node1.Server.UpdateConfig(context.Background(), &rpc.UpdateConfigRequest{
			Key:   "tls_mode",
			Value: config.TLSModeRequired,
		})
		return err == nil && resp != nil && resp.Success
	}, 20*time.Second, 500*time.Millisecond,
		"the flip never became possible; the last refusal was: %s", resp.GetMessage())
	require.True(t, resp.Success,
		"a healthy cluster with both certificates published refused the flip: %s", resp.Message)
	require.NotContains(t, resp.Message, "except",
		"a node was not told about the flip and is now cut off: %s", resp.Message)

	// Both nodes on the new terms. node2 learns it from the delivery, which is
	// the half that has to travel over the plaintext wire the flip removes.
	for _, n := range []*testutils.TestNode{node1, node2} {
		node := n
		require.Eventually(t, func() bool {
			cfg := node.Server.GetMemberList().Config()
			return cfg != nil && cfg.Pulse.TLSRequired()
		}, 15*time.Second, 250*time.Millisecond,
			"%s did not take the flip; it is now serving plaintext to a peer that has "+
				"stopped accepting it", node.Hostname)
	}

	// And still talking. This is what the connection caches broke: the listeners
	// rebind and every log line reads correctly while the pooled connections stay
	// plaintext and nothing ever replaces them, so the cluster only falls apart
	// once the health checks have had time to fail.
	time.Sleep(2 * time.Second)

	requireMemberStatus(t, node1, node2.Hostname, "passive",
		"node2 stopped being reachable from node1 after the flip")
	requireMemberStatus(t, node2, node1.Hostname, "active",
		"node1 stopped being reachable from node2 after the flip")

	// A config change still propagates, over TLS this time, which is the proof
	// that the peer connections were rebuilt rather than merely re-listened for.
	_, err = node1.Server.UpdateConfig(context.Background(), &rpc.UpdateConfigRequest{
		Key:   "fo_limit",
		Value: "12345",
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		cfg := node2.Server.GetMemberList().Config()
		return cfg != nil && cfg.Pulse.FailOverLimit == 12345
	}, 15*time.Second, 250*time.Millisecond,
		"a config change made after the flip never reached node2 over TLS; the peer "+
			"connections are still the plaintext ones made before it")
}
