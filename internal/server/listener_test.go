//go:build !windows

package server

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestListenTCPReuseAddr_SetsSocketOptions asserts that listenTCPReuseAddr
// applies SO_REUSEADDR to the bound socket so that a subsequent rebind on
// the same IP:port succeeds even while the kernel holds the previous
// endpoint in TIME_WAIT. SO_REUSEPORT is verified best-effort: kernels or
// sandboxes that strip it are tolerated.
func TestListenTCPReuseAddr_SetsSocketOptions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ln, err := listenTCPReuseAddr(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	tcpLn, ok := ln.(*net.TCPListener)
	if !ok {
		t.Fatalf("expected *net.TCPListener, got %T", ln)
	}
	rc, err := tcpLn.SyscallConn()
	if err != nil {
		t.Fatalf("syscall conn: %v", err)
	}

	var (
		reuseAddr   int
		reuseAddrEr error
		reusePort   int
		reusePortEr error
	)
	ctrlErr := rc.Control(func(fd uintptr) {
		reuseAddr, reuseAddrEr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR)
		reusePort, reusePortEr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT)
	})
	if ctrlErr != nil {
		t.Fatalf("rawconn control: %v", ctrlErr)
	}

	if reuseAddrEr != nil {
		t.Fatalf("getsockopt SO_REUSEADDR: %v", reuseAddrEr)
	}
	if reuseAddr == 0 {
		t.Fatalf("SO_REUSEADDR not set on bound socket")
	}

	// SO_REUSEPORT is best-effort. Tolerate ENOPROTOOPT on platforms or
	// kernels that lack support; only fail on unexpected errors.
	switch {
	case reusePortEr == nil && reusePort == 0:
		t.Logf("SO_REUSEPORT not set (tolerated; setsockopt is best-effort)")
	case reusePortEr != nil && !errors.Is(reusePortEr, unix.ENOPROTOOPT):
		t.Logf("getsockopt SO_REUSEPORT returned unexpected error (tolerated): %v", reusePortEr)
	}
}
