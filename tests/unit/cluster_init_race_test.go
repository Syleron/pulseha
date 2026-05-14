package unit

import (
	"context"
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

// newTestServer mirrors the bootstrap idiom used by create_cluster_deadlock_test.go
// and reconfigure_concurrent_test.go: PULSEHA_TEST mode, a temp cert dir, and a
// Server built directly without Start() to avoid binding default sockets.
func newTestServer(t *testing.T) *server.Server {
	t.Helper()
	_ = os.Setenv("PULSEHA_TEST", "true")
	security.CertDir = t.TempDir()

	cfg, err := config.New()
	if err != nil {
		t.Fatalf("config.New: %v", err)
	}
	logger := log.New(os.Stdout)
	logger.SetLevel(log.WarnLevel)

	ml := membership.NewMemberList(cfg, logger)
	hc := membership.NewHealthChecker(ml, logger)
	return server.NewServer(cfg, logger, ml, hc)
}

// TestClusterInit_PostCreate_Rejects_CreateClusterAndInitiateJoin verifies that
// once a cluster is configured, concurrent CreateCluster and InitiateJoin
// requests both fall through the clusterInitMu-guarded rejection path with the
// documented messages. Exercises the post-PR-#226 WARN-on-rejection branches.
func TestClusterInit_PostCreate_Rejects_CreateClusterAndInitiateJoin(t *testing.T) {
	s := newTestServer(t)

	ctx := context.Background()
	createResp, err := s.CreateCluster(ctx, &rpc.CreateClusterRequest{
		BindIp:   "127.0.0.1",
		BindPort: "0",
		Mode:     "active-passive",
	})
	if err != nil {
		t.Fatalf("initial CreateCluster error: %v", err)
	}
	if createResp == nil || !createResp.Success {
		t.Fatalf("initial CreateCluster failed: %+v", createResp)
	}

	type result struct {
		createResp *rpc.CreateClusterResponse
		joinResp   *rpc.InitiateJoinResponse
		createErr  error
		joinErr    error
	}
	var r result
	var wg sync.WaitGroup
	wg.Add(2)

	done := make(chan struct{})
	go func() {
		defer wg.Done()
		r.createResp, r.createErr = s.CreateCluster(ctx, &rpc.CreateClusterRequest{
			BindIp:   "127.0.0.1",
			BindPort: "0",
			Mode:     "active-passive",
		})
	}()
	go func() {
		defer wg.Done()
		r.joinResp, r.joinErr = s.InitiateJoin(ctx, &rpc.InitiateJoinRequest{
			TargetHost: "127.0.0.1",
		})
	}()
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent CreateCluster/InitiateJoin did not return within 5s (possible deadlock on clusterInitMu)")
	}

	if r.createErr != nil {
		t.Fatalf("CreateCluster returned error: %v", r.createErr)
	}
	if r.joinErr != nil {
		t.Fatalf("InitiateJoin returned error: %v", r.joinErr)
	}
	if r.createResp == nil || r.createResp.Success {
		t.Fatalf("CreateCluster should have been rejected, got: %+v", r.createResp)
	}
	if got, want := r.createResp.Message, "cluster is already configured"; got != want {
		t.Errorf("CreateCluster rejection message: got %q, want %q", got, want)
	}
	if r.joinResp == nil || r.joinResp.Success {
		t.Fatalf("InitiateJoin should have been rejected, got: %+v", r.joinResp)
	}
	if got, want := r.joinResp.Message, "node is already part of a cluster; leave first"; got != want {
		t.Errorf("InitiateJoin rejection message: got %q, want %q", got, want)
	}

	// The pre-existing single node should still be the only one in config.
	cfg := s.GetMemberList() // sanity that the server is still usable
	if cfg == nil {
		t.Fatal("memberList should be non-nil after rejected calls")
	}
}

// TestClusterInit_HandleNodeJoinBlocksDuringCreateCluster is the regression
// test for PR #226's TOCTOU fix. It directly verifies that HandleNodeJoin
// acquires clusterInitMu by observing that it blocks while CreateCluster (which
// also holds clusterInitMu for its full duration) is running.
//
// In-process Go scheduling makes "fire two goroutines and hope they race"
// unreliable: the TOCTOU window between memberList.GetMemberCount() and the
// subsequent AddMember is too tight to interleave deterministically without
// real gRPC overhead. This timing approach is deterministic instead — with
// the mutex, HandleNodeJoin must wait ~the full CreateCluster duration; without
// it, HandleNodeJoin completes in ms regardless of CreateCluster's state.
//
// After both return, HandleNodeJoin must have taken the existing-cluster path
// (CreateCluster already added a node), confirming serialization at the
// behavioural level too.
func TestClusterInit_HandleNodeJoinBlocksDuringCreateCluster(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	createStart := time.Now()
	createDone := make(chan struct{})
	var createResp *rpc.CreateClusterResponse
	var createErr error
	go func() {
		defer close(createDone)
		createResp, createErr = s.CreateCluster(ctx, &rpc.CreateClusterRequest{
			BindIp:   "127.0.0.1",
			BindPort: "0",
			Mode:     "active-passive",
		})
	}()

	// Wait until CreateCluster has plausibly acquired clusterInitMu. The mutex
	// is acquired near the top of the function (server.go:3201), so a short
	// sleep is sufficient. If this turns out to be flaky on slow systems,
	// extend the wait — the test's correctness depends on HandleNodeJoin
	// arriving *after* CreateCluster has the lock.
	time.Sleep(20 * time.Millisecond)

	joinStart := time.Now()
	joinDone := make(chan struct{})
	var joinResp *rpc.JoinResponse
	var joinErr error
	var joinDuration time.Duration
	go func() {
		defer close(joinDone)
		joinResp, joinErr = s.HandleNodeJoin(ctx, &rpc.JoinRequest{
			NodeId:   "joiner-1",
			Hostname: "joiner-host",
			Token:    "",
			BindIp:   "127.0.0.1",
			BindPort: "20001",
		})
		// Capture HandleNodeJoin's actual wall-clock duration before the
		// channel close fires. Measuring outside the goroutine would include
		// any time the test spends waiting on createDone afterwards.
		joinDuration = time.Since(joinStart)
	}()

	select {
	case <-createDone:
	case <-time.After(10 * time.Second):
		t.Fatal("CreateCluster did not return within 10s")
	}
	createDuration := time.Since(createStart)

	select {
	case <-joinDone:
	case <-time.After(10 * time.Second):
		t.Fatal("HandleNodeJoin did not return within 10s (deadlock?)")
	}

	if createErr != nil {
		t.Fatalf("CreateCluster error: %v", createErr)
	}
	if createResp == nil || !createResp.Success {
		t.Fatalf("CreateCluster failed: %+v", createResp)
	}
	if joinErr != nil {
		t.Fatalf("HandleNodeJoin error: %v", joinErr)
	}
	if joinResp == nil || !joinResp.Success {
		t.Fatalf("HandleNodeJoin failed: %+v", joinResp)
	}

	// Regression assertion: HandleNodeJoin must have waited for CreateCluster
	// to release clusterInitMu. We require its duration to be at least half of
	// CreateCluster's — generous enough to tolerate scheduler jitter, strict
	// enough that a missing mutex (HandleNodeJoin returning in ms while
	// CreateCluster takes hundreds of ms) fails the assertion.
	minExpectedJoinDuration := createDuration / 2
	if joinDuration < minExpectedJoinDuration {
		t.Errorf("HandleNodeJoin returned in %v while CreateCluster took %v; "+
			"expected join to block on clusterInitMu (min %v). Regression: clusterInitMu "+
			"is not being acquired by HandleNodeJoin.", joinDuration, createDuration, minExpectedJoinDuration)
	}

	// Behavioural confirmation: HandleNodeJoin must have taken the
	// existing-cluster path (CreateCluster's node was already in memberList
	// by the time HandleNodeJoin got the lock).
	if got, want := joinResp.Message, "Successfully joined cluster"; got != want {
		t.Errorf("HandleNodeJoin message: got %q, want %q (HandleNodeJoin should not have taken the first-node path)", got, want)
	}

	if got := s.GetMemberList().GetMemberCount(); got != 2 {
		t.Errorf("memberList count: got %d, want 2 (one from CreateCluster, one from HandleNodeJoin)", got)
	}
}
