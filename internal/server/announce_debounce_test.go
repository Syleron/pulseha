package server

import (
	"io"
	"testing"
	"time"

	log "github.com/charmbracelet/log"
)

// The demotion detector that drives most announcements has to accept a peer arriving from
// Unknown, because that is what a healed partition produces — and it is also what a merely slow
// peer produces, over and over: a node doing bulk IP work is slow enough to answer that its
// peers mark it Unknown and then Passive again a tick later (docs/TEST-PLAN.md #2/#26).
//
// Unbounded, each flap re-places and re-announces the whole group. On the 201-address topology
// that is #4's per-address arping cost paid on a peer's health-check jitter, indefinitely.
func TestAnnounceIsDebounced(t *testing.T) {
	s := &Server{logger: log.New(io.Discard)}

	if !s.allowAnnounce() {
		t.Fatal("expected the first announcement to be allowed")
	}
	for i := 0; i < 5; i++ {
		if s.allowAnnounce() {
			t.Errorf("announcement %d was allowed inside the debounce window", i+2)
		}
	}

	// Once the window has passed, the next one must get through — the guard is a rate
	// limit, not a latch.
	s.announceMu.Lock()
	s.lastAnnounce = time.Now().Add(-announceDebounceInterval - time.Second)
	s.announceMu.Unlock()

	if !s.allowAnnounce() {
		t.Error("expected an announcement to be allowed once the window elapsed")
	}
}

// The window has to be long enough to absorb a flap and short enough that a real second
// demotion is not swallowed for minutes.
func TestAnnounceDebounceWindowIsSane(t *testing.T) {
	if announceDebounceInterval < 5*time.Second {
		t.Errorf("announceDebounceInterval = %v, too short to absorb a health-check flap",
			announceDebounceInterval)
	}
	if announceDebounceInterval > 2*time.Minute {
		t.Errorf("announceDebounceInterval = %v, long enough to swallow a genuine second demotion",
			announceDebounceInterval)
	}
}
