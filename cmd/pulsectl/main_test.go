package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syleron/pulseha/packages/config"
)

// run executes the real root command with args and returns what cobra itself
// wrote and the error main would print.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	err := rootCmd.Execute()
	return out.String(), err
}

func noDaemon(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pha") // short: a socket path is capped at 104 bytes on macOS
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	prev := config.CLI_SOCKET_PATH
	config.CLI_SOCKET_PATH = filepath.Join(dir, "pulseha.sock")
	t.Cleanup(func() { config.CLI_SOCKET_PATH = prev })
}

// Regression for GitHub #19. With the daemon down, `pulsectl status` printed the
// raw gRPC transport error, then the command's usage — as if it had been typed
// wrong — and then main printed the error a second time.
func TestAnUnreachableDaemonIsOneSentenceAndNoUsage(t *testing.T) {
	noDaemon(t)

	printed, err := run(t, "status")

	if err == nil {
		t.Fatal("status succeeded with no daemon")
	}
	if !strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "rpc error:") {
		t.Errorf("error = %q, want the plain explanation", err)
	}
	if strings.Contains(printed, "Usage:") {
		t.Errorf("usage printed after a runtime failure:\n%s", printed)
	}
	if strings.Contains(printed, err.Error()) {
		t.Errorf("cobra printed the error itself; main prints it, so it would appear twice:\n%s", printed)
	}
}

// A mistake on the command line is still answered with the usage.
func TestAMissingRequiredFlagStillShowsTheUsage(t *testing.T) {
	printed, err := run(t, "node", "promote")

	if err == nil || !strings.Contains(err.Error(), `required flag(s) "node-id" not set`) {
		t.Fatalf("err = %v, want the missing-flag error", err)
	}
	if !strings.Contains(printed, "Usage:") {
		t.Errorf("no usage for a missing required flag:\n%s", printed)
	}
}
