package client

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/rpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// shortDir is a temporary directory with a path short enough for a Unix socket:
// macOS caps sun_path at 104 bytes, and t.TempDir() there is longer than that.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pha")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// withSocket points the CLI client at path for one test.
func withSocket(t *testing.T, path string) {
	t.Helper()
	prev := config.CLI_SOCKET_PATH
	config.CLI_SOCKET_PATH = path
	t.Cleanup(func() { config.CLI_SOCKET_PATH = prev })
}

func statusCall(t *testing.T) error {
	t.Helper()
	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = c.CLI().Status(ctx, &rpc.StatusRequest{})
	if err == nil {
		t.Fatal("Status succeeded with no daemon behind the socket")
	}
	return err
}

func assertExplained(t *testing.T, err error, code codes.Code, want ...string) {
	t.Helper()
	msg := err.Error()
	if strings.Contains(msg, "rpc error:") || strings.Contains(msg, "desc =") {
		t.Errorf("message still carries the gRPC wrapper: %q", msg)
	}
	for _, w := range want {
		if !strings.Contains(msg, w) {
			t.Errorf("message %q does not mention %q", msg, w)
		}
	}
	if got := status.Code(err); got != code {
		t.Errorf("status.Code = %v, want %v: callers that branch on the code must still see it", got, code)
	}
}

// Regression for GitHub #19. With the daemon not running, every pulsectl command
// printed
//
//	rpc error: code = Unavailable desc = connection error: desc = "transport:
//	Error while dialing: dial unix /root/.pulseha/pulseha.sock: connect: no such
//	file or directory"
//
// — the one useful fact, that the daemon is not there, buried at the end.
func TestTheCLISaysPlainlyWhenTheDaemonSocketIsMissing(t *testing.T) {
	sock := filepath.Join(shortDir(t), "pulseha.sock")
	withSocket(t, sock)

	assertExplained(t, statusCall(t), codes.Unavailable, sock, "does not exist", "systemctl status pulseha")
}

// A socket file with nothing listening — a daemon that died without removing it,
// or one still starting.
func TestTheCLISaysPlainlyWhenNothingListensOnTheSocket(t *testing.T) {
	sock := filepath.Join(shortDir(t), "pulseha.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.SetUnlinkOnClose(false) // leave the file behind, as a dead daemon does
	ln.Close()
	withSocket(t, sock)

	assertExplained(t, statusCall(t), codes.Unavailable, sock, "nothing is listening", "systemctl status pulseha")
}

func TestExplainCLIError(t *testing.T) {
	const sock = "/var/run/pulseha/pulseha.sock"
	for _, tc := range []struct {
		name string
		in   error
		code codes.Code
		want []string
	}{
		{"permission denied on the socket",
			status.Error(codes.Unavailable, `connection error: desc = "transport: Error while dialing: dial unix `+sock+`: connect: permission denied"`),
			codes.Unavailable, []string{"permission denied", sock, "sudo"}},
		{"the daemon took too long",
			status.Error(codes.DeadlineExceeded, "context deadline exceeded"),
			codes.DeadlineExceeded, []string{"did not answer in time", "journalctl -t pulseha"}},
		{"a daemon that does not know the command",
			status.Error(codes.Unimplemented, "unknown method Witness for service rpc.CLI"),
			codes.Unimplemented, []string{"does not support this command", "versions"}},
		{"the daemon's own refusal keeps its words",
			status.Error(codes.FailedPrecondition, "no cluster configured"),
			codes.FailedPrecondition, []string{"no cluster configured"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertExplained(t, explainCLIError(sock, tc.in), tc.code, tc.want...)
		})
	}
}

// Not a gRPC error at all — nothing to explain, and nothing to lose.
func TestExplainCLIErrorPassesOtherErrorsThrough(t *testing.T) {
	plain := errors.New("plain")
	if got := explainCLIError("/x.sock", plain); got != plain {
		t.Errorf("explainCLIError changed a non-gRPC error: %v", got)
	}
	if explainCLIError("/x.sock", nil) != nil {
		t.Error("explainCLIError(nil) != nil")
	}
}
