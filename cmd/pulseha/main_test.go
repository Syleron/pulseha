package main

import (
	"bytes"
	"strings"
	"testing"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/packages/config"
)

// attemptedSyslog reports whether setupLogging tried to open syslog, by reading
// the two lines it emits either way -- one on success, one on failure.
//
// The attempt is the right thing to assert on, and the outcome is not: on a host
// with a working syslog both a correct and an incorrect decision end in silence,
// which is how #110 survived. This discriminates on a developer's machine, on a
// CI runner and in a bare container alike.
//
// setupLogging redirects the logger at the end, but both lines are written before
// that, so the buffer still sees them.
func attemptedSyslog(t *testing.T, pulse config.Local) bool {
	t.Helper()

	var buf bytes.Buffer
	logger := log.New(&buf)
	logger.SetLevel(log.DebugLevel)

	cfg := &config.Config{Pulse: pulse}
	if err := setupLogging(cfg, logger); err != nil {
		t.Fatalf("setupLogging: %v", err)
	}

	out := buf.String()
	return strings.Contains(out, "Syslog logging enabled") ||
		strings.Contains(out, "Failed to connect to syslog")
}

// Regression for docs/TEST-PLAN.md defect #110, at the site that actually did the
// damage.
//
// setupLogging re-derived the decision -- "the tag is empty, so this must be an
// old config, so syslog must be on" -- which is also what a current config looks
// like when nobody set a tag. So `log_to_syslog: false` was ignored and the node
// logged to syslog anyway.
func TestSyslogDisabledIsNotAttempted(t *testing.T) {
	if attemptedSyslog(t, config.Local{LogToSyslog: false}) {
		t.Error("setupLogging opened syslog with log_to_syslog false; turning it " +
			"off in the config has to turn it off")
	}
}

// The same instruction with every other syslog field left empty, which is the
// exact shape both overrides read as "old config, turn syslog on" -- so an
// operator who set only log_to_syslog false got the one config guaranteed to
// ignore them.
func TestSyslogDisabledIsNotAttemptedWithNoOtherFieldsSet(t *testing.T) {
	if attemptedSyslog(t, config.Local{
		LogToSyslog:    false,
		SyslogTag:      "",
		SyslogFacility: "",
		SyslogNetwork:  "",
		SyslogAddress:  "",
	}) {
		t.Error("an empty tag was read as consent to enable syslog")
	}
}

// The positive control, and it is what makes the two above mean anything: a pass
// that never reaches the syslog branch would satisfy them by doing nothing at
// all. This is host-independent for the same reason -- enabled produces one of
// the two lines whichever way the connection goes.
func TestSyslogEnabledIsAttempted(t *testing.T) {
	if !attemptedSyslog(t, config.Local{LogToSyslog: true, SyslogTag: "pulseha"}) {
		t.Error("setupLogging did not try syslog with log_to_syslog true; the " +
			"disabled-case tests above would then pass vacuously")
	}
}
