//go:build linux

package server

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestListenTCPReuseAddr_RapidRebind verifies the bug fix: closing a
// listener and immediately re-binding the same address must succeed.
// Without SO_REUSEADDR this commonly fails with EADDRINUSE while the
// previous socket is in TIME_WAIT, which is the failure mode that surfaces
// as "Async reconfigure failed after ConfigSync: bind: address already in
// use" on the cluster listener after ConfigSync.
func TestListenTCPReuseAddr_RapidRebind(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	first, err := listenTCPReuseAddr(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("first listen failed: %v", err)
	}
	addr := first.Addr().String()

	// Establish + close a client connection so the socket has a chance to
	// enter TIME_WAIT, mirroring what gRPC's Serve loop produces.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial first listener failed: %v", err)
	}
	_ = conn.Close()

	if err := first.Close(); err != nil {
		t.Fatalf("close first listener failed: %v", err)
	}

	second, err := listenTCPReuseAddr(ctx, addr)
	if err != nil {
		t.Fatalf("rebind to %s failed: %v", addr, err)
	}
	_ = second.Close()
}
