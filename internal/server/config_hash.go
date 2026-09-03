package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/syleron/pulseha/packages/config"
)

// sharedConfigHash fingerprints the part of a config that ConfigSync actually
// propagates, so two converged nodes agree on it and a diverged one does not.
//
// This exists because the config generation cannot detect divergence. A generation
// only advances on a mutation, and the envelope syncs that carry epoch, leader and
// member states advance nothing. So a node that misses the single full-config
// broadcast for a join sits at the *same* generation as its peers with a different
// member map, and the once-a-minute reconcile is documented to skip exactly that
// case: "a peer already holding this generation ignores the message". That is
// docs/TEST-PLAN.md #103 — divergence at an equal generation was permanent and,
// worse, invisible. The hash is what makes it visible.
//
// It covers everything the receiver adopts and nothing it preserves. The nine
// fields zeroed below are the ones ConfigSync's apply path explicitly keeps from
// the local config -- identity and the logging/syslog settings -- and they
// legitimately differ between nodes. Including any of them would make every node's
// hash differ from every other's forever, turning the detector into a permanent
// false alarm and the repair into a pull storm. If that preserve list changes, this
// must change with it; the two are checked against each other by
// TestTheHashCoversWhatConfigSyncAdopts.
//
// Group and IPGroups member order is sorted rather than hashed as-is. A wholesale
// apply copies slices verbatim so order does usually match, but a local mutation
// appends, and two nodes holding the same addresses in a different order are not
// diverged. Sorting a throwaway copy removes that false-positive class without
// touching the config anyone serves from.
func sharedConfigHash(cfg *config.Config) (string, error) {
	if cfg == nil {
		return "", nil
	}

	// A JSON round-trip is the deep copy: it follows the struct as it grows, and
	// nothing below may write to the config this node is running on.
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	var shared config.Config
	if err := json.Unmarshal(raw, &shared); err != nil {
		return "", err
	}

	// Locally preserved by ConfigSync's apply path -- see the preserve block in
	// the isFullConfig branch of ConfigSync.
	shared.Pulse.LocalNode = ""
	shared.Pulse.LoggingLevel = ""
	shared.Pulse.LogToFile = false
	shared.Pulse.LogFileLocation = ""
	shared.Pulse.LogToSyslog = false
	shared.Pulse.SyslogNetwork = ""
	shared.Pulse.SyslogAddress = ""
	shared.Pulse.SyslogFacility = ""
	shared.Pulse.SyslogTag = ""

	for _, ips := range shared.Groups {
		sort.Strings(ips)
	}
	for _, node := range shared.Nodes {
		if node == nil {
			continue
		}
		for _, groups := range node.IPGroups {
			sort.Strings(groups)
		}
	}

	// json.Marshal sorts map keys, so this is canonical for a given content.
	canonical, err := json.Marshal(&shared)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// shortHash trims a hash for logging. Full hashes make a log line unreadable and
// the prefix is plenty to tell two apart by eye.
func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}
