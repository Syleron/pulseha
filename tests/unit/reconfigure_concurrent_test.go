package unit

import (
	contextpkg "context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/internal/server"
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/security"
	rpc "github.com/syleron/pulseha/rpc"
)

// TestReconfigureConcurrent_NoBindRace asserts that concurrent Reconfigure()
// calls serialize on reconfigureMu so they don't race on the cluster listener
// rebind. Without the mutex, the second goroutine to reach startClusterListener
// reliably fails with "bind: address already in use" because both target the
// same fixed port that was written back to config during CreateCluster.
func TestReconfigureConcurrent_NoBindRace(t *testing.T) {
	_ = os.Setenv("PULSEHA_TEST", "true")

	tmpDir := t.TempDir()
	security.CertDir = tmpDir

	cfg, err := config.New()
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}
	logger := log.New(os.Stdout)
	logger.SetLevel(log.WarnLevel)

	ml := membership.NewMemberList(cfg, logger)
	hc := membership.NewHealthChecker(ml, logger)
	s := server.NewServer(cfg, logger, ml, hc)

	// Bring up the initial cluster listener on an ephemeral port; the
	// resolved port is written back into cfg.Nodes[localID].Port and
	// (because PULSEHA_TEST=true makes config.Reload a no-op) will be the
	// same fixed target for every subsequent Reconfigure().
	ctx, cancel := contextpkg.WithTimeout(contextpkg.Background(), 5*time.Second)
	defer cancel()
	resp, err := s.CreateCluster(ctx, &rpc.CreateClusterRequest{
		BindIp:   "127.0.0.1",
		BindPort: "0",
		Mode:     "active-passive",
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if resp == nil || !resp.Success {
		t.Fatalf("CreateCluster unsuccessful: %+v", resp)
	}

	const goroutines = 8
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		errs  = make([]error, goroutines)
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			errs[idx] = s.Reconfigure()
		}(i)
	}

	close(start)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("concurrent Reconfigure() did not complete within timeout")
	}

	for i, e := range errs {
		if e != nil {
			t.Errorf("goroutine %d Reconfigure returned error: %v", i, e)
		}
	}
}
