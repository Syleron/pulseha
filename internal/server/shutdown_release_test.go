package server

import (
	"io"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/packages/config"
)

// Regression for GitHub #273 (docs/TEST-PLAN.md defect #119). Server.Stop took
// the node's floating IPs down *before* stopping the IP monitor, and the monitor
// answered every delete with "expected IP removed from Active node; restoring".
// On the two-node pair all 118 addresses came straight back, the old process
// exited holding them, and the peer took the group over as well: 10-13s with
// every address on both nodes, in 3 of 5 restarts.
//
// So every release must happen with the monitor already stopped.
func TestStopReleasesFloatingIPsOnlyAfterTheIPMonitorHasStopped(t *testing.T) {
	t.Setenv("PULSEHA_TEST", "true")
	prevLocation := config.CONFIG_LOCATION
	config.CONFIG_LOCATION = filepath.Join(t.TempDir(), "config.json")
	t.Cleanup(func() { config.CONFIG_LOCATION = prevLocation })

	const localID = "local-node"
	cfg := &config.Config{
		Pulse: config.Local{
			Mode:                "active-passive",
			LocalNode:           localID,
			HealthCheckInterval: 1000,
			FailOverInterval:    5000,
			FailOverLimit:       10000,
		},
		Groups: map[string][]string{"group1": {"10.0.0.1/24", "10.0.0.2/24"}},
		Nodes: map[string]*config.Node{
			localID: {
				Hostname: localID,
				IP:       "127.0.0.1",
				Port:     "49151",
				IPGroups: map[string][]string{"eth0": {"group1"}},
			},
		},
	}

	logger := log.New(io.Discard)
	ml := membership.NewMemberList(cfg, logger)
	if err := ml.AddMemberQuiet(localID); err != nil {
		t.Fatalf("AddMemberQuiet: %v", err)
	}
	ml.GetMemberByID(localID).Status = membership.StatusActive

	monitor := membership.NewIPMonitor(ml, logger)
	s := &Server{config: cfg, logger: logger, memberList: ml, ipMonitor: monitor}

	var (
		mu               sync.Mutex
		released         []string
		releasedTooEarly []string
	)
	s.releaseIP = func(iface, ip string) error {
		mu.Lock()
		defer mu.Unlock()
		released = append(released, iface+" "+ip)
		if !monitor.Stopped() {
			releasedTooEarly = append(releasedTooEarly, ip)
		}
		return nil
	}

	s.Stop()

	slices.Sort(released)
	if want := []string{"eth0 10.0.0.1/24", "eth0 10.0.0.2/24"}; !slices.Equal(released, want) {
		t.Fatalf("released %v, want %v", released, want)
	}
	if len(releasedTooEarly) > 0 {
		t.Errorf("released %v while the IP monitor was still running; it restores each one as it goes", releasedTooEarly)
	}
}
