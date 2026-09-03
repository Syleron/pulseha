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
// It covers everything the receiver adopts and nothing it preserves. The fields in
// locallyPreservedFields are the ones ConfigSync's apply path explicitly keeps from
// the local config -- identity and the logging/syslog settings -- and they
// legitimately differ between nodes. Including any of them would make every node's
// hash differ from every other's forever, turning the detector into a permanent
// false alarm and the repair into a pull storm. If that preserve list changes, this
// must change with it; the two are checked against each other by
// TestTheHashCoversWhatConfigSyncAdopts.
//
// Everything else about the shape of the comparison is in canonicalise, which is
// where the differences that are not divergence get removed. It works on decoded
// JSON rather than the struct so that nothing here can write to the config this
// node is running on.
func sharedConfigHash(cfg *config.Config) (string, error) {
	if cfg == nil {
		return "", nil
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", err
	}

	// Locally preserved by ConfigSync's apply path -- see the preserve block in
	// the isFullConfig branch of ConfigSync. Deleted rather than zeroed: the
	// comparison only has to be consistent between nodes, and a deleted key cannot
	// be confused with a value some node genuinely holds.
	if pulse, ok := decoded[localSettingsKey].(map[string]interface{}); ok {
		for _, field := range locallyPreservedFields {
			delete(pulse, field)
		}
	}

	canonicalise(decoded)

	// json.Marshal sorts map keys, so this is canonical for a given content.
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// localSettingsKey is the JSON name of config.Config.Pulse.
const localSettingsKey = "pulseha"

// locallyPreservedFields are the JSON names of the fields ConfigSync keeps from
// the local config rather than adopting from a peer. Kept in step with that
// preserve block by TestTheHashCoversWhatConfigSyncAdopts.
var locallyPreservedFields = []string{
	"local_node",
	"logging_level",
	"log_to_file",
	"log_file_location",
	"log_to_syslog",
	"syslog_network",
	"syslog_address",
	"syslog_facility",
	"syslog_tag",
}

// canonicalise makes two configs that hold the same thing hash the same thing.
//
// Two normalisations, both learned from a test rather than guessed at:
//
// Absent, null and empty are one state. A config that has never had a plugin
// marshals `"plugins": null`; the same config after a round trip through a sync
// payload marshals `"plugins": {}`. Nothing about the cluster differs, but the
// bytes do -- and since a repair *is* a round trip, the repaired node's hash would
// disagree with the coordinator's the moment it finished agreeing with it, and it
// would pull another repair every interval forever. An empty map, an empty slice
// and a null are therefore all dropped.
//
// String slice order does not count. A wholesale apply copies slices verbatim so
// order usually matches, but a local mutation appends, and two nodes holding the
// same addresses in a different order are not diverged.
//
// The cost of the first rule is that an explicitly empty group hashes the same as
// no group at all. That is a real blind spot and a small one: an empty group holds
// nothing to diverge about, where nil-versus-empty was a false positive on every
// comparison.
func canonicalise(node map[string]interface{}) {
	for key, value := range node {
		switch v := value.(type) {
		case nil:
			delete(node, key)
		case map[string]interface{}:
			canonicalise(v)
			if len(v) == 0 {
				delete(node, key)
			}
		case []interface{}:
			if len(v) == 0 {
				delete(node, key)
				continue
			}
			allStrings := true
			for _, item := range v {
				if _, ok := item.(string); !ok {
					allStrings = false
					break
				}
			}
			if allStrings {
				sort.Slice(v, func(i, j int) bool {
					return v[i].(string) < v[j].(string)
				})
			}
		}
	}
}

// shortHash trims a hash for logging. Full hashes make a log line unreadable and
// the prefix is plenty to tell two apart by eye.
func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}
