package server

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"sort"
	"testing"

	"github.com/syleron/pulseha/packages/config"
)

// hashTestConfig is a two-node cluster with one group, enough for a hash to have
// something to cover.
func hashTestConfig() *config.Config {
	return &config.Config{
		Pulse: config.Local{
			Mode:                "active-active",
			LocalNode:           "node-a",
			ClusterToken:        "shared-token",
			HealthCheckInterval: 1000,
			FailOverInterval:    5000,
			FailOverLimit:       10000,
			LoggingLevel:        "info",
		},
		Groups: map[string][]string{"group1": {"10.0.0.1/24", "10.0.0.2/24"}},
		Nodes: map[string]*config.Node{
			"node-a": {Hostname: "host-a", IP: "10.0.0.10", Port: "8443",
				IPGroups: map[string][]string{"eth0": {"group1"}}},
			"node-b": {Hostname: "host-b", IP: "10.0.0.11", Port: "8443",
				IPGroups: map[string][]string{"eth0": {"group1"}}},
		},
	}
}

func hashOf(t *testing.T, cfg *config.Config) string {
	t.Helper()
	h, err := sharedConfigHash(cfg)
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	if h == "" {
		t.Fatal("hashed to the empty string, which compares equal to everything")
	}
	return h
}

// TestTheHashCoversWhatConfigSyncAdopts is the coupling that keeps the two lists
// in step, and the reason it is a source scan rather than a literal.
//
// sharedConfigHash must exclude exactly the fields ConfigSync's apply path
// preserves from the local config. Exclude too few and every node's hash differs
// forever, so the detector cries divergence at a converged cluster and every peer
// pulls a config it already has. Exclude too many and a genuine difference in a
// cluster-wide setting hashes equal, so the detector is blind to it.
//
// Nothing in the type system ties those two lists together: one is a block of
// assignments 2,000 lines away in ConfigSync, the other a block of zeroing in
// sharedConfigHash. So this reads the first out of the source and compares.
func TestTheHashCoversWhatConfigSyncAdopts(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("reading server.go: %v", err)
	}
	// The apply path's restore block: `newConfig.Pulse.X = xPreserve`.
	re := regexp.MustCompile(`newConfig\.Pulse\.([A-Za-z]+) = [a-zA-Z]+Preserve`)
	matches := re.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatal("found no `newConfig.Pulse.X = xPreserve` assignments in server.go; " +
			"the preserve block has been rewritten and this test can no longer see " +
			"it, so it is not checking anything")
	}
	seen := map[string]bool{}
	var preserved []string
	for _, m := range matches {
		if !seen[m[1]] {
			seen[m[1]] = true
			preserved = append(preserved, m[1])
		}
	}
	sort.Strings(preserved)

	// A field is excluded from the hash iff changing it leaves the hash alone.
	var excluded []string
	base := hashOf(t, hashTestConfig())
	for _, name := range preserved {
		cfg := hashTestConfig()
		if !setLocalFieldForTest(t, &cfg.Pulse, name) {
			t.Fatalf("Local has no field %q, but server.go preserves it", name)
		}
		if hashOf(t, cfg) == base {
			excluded = append(excluded, name)
		}
	}
	sort.Strings(excluded)

	if !reflect.DeepEqual(preserved, excluded) {
		t.Errorf("ConfigSync preserves %v but sharedConfigHash excludes %v.\n"+
			"A field preserved locally but included in the hash makes every node's "+
			"hash differ permanently, so a converged cluster reports divergence and "+
			"every peer pulls a config it already has", preserved, excluded)
	}
}

// setLocalFieldForTest gives the named field a value different from the one
// hashTestConfig set, so the hash has something to notice. Reports whether the
// field exists.
func setLocalFieldForTest(t *testing.T, local *config.Local, name string) bool {
	t.Helper()
	f := reflect.ValueOf(local).Elem().FieldByName(name)
	if !f.IsValid() || !f.CanSet() {
		return false
	}
	switch f.Kind() {
	case reflect.String:
		f.SetString("changed-by-the-test")
	case reflect.Bool:
		f.SetBool(!f.Bool())
	case reflect.Int:
		f.SetInt(f.Int() + 1)
	default:
		t.Fatalf("field %q has kind %s, which this test cannot vary", name, f.Kind())
	}
	return true
}

// TestClusterWideSettingsDoChangeTheHash is the other half, and the positive
// control for the test above: zeroing everything would satisfy an exclusion check
// trivially, so the fields that must *not* be excluded are named here.
//
// Mode is the one that matters most. Two nodes disagreeing about active-passive
// versus active-active is the worst divergence this repo has (docs/TEST-PLAN.md
// #22), and it has to be detectable.
func TestClusterWideSettingsDoChangeTheHash(t *testing.T) {
	base := hashOf(t, hashTestConfig())
	for _, name := range []string{
		"Mode", "ClusterToken", "HealthCheckInterval", "FailOverInterval",
		"FailOverLimit", "AutoFailback",
	} {
		cfg := hashTestConfig()
		if !setLocalFieldForTest(t, &cfg.Pulse, name) {
			t.Fatalf("Local has no field %q", name)
		}
		if hashOf(t, cfg) == base {
			t.Errorf("changing Pulse.%s left the hash alone; it is cluster-wide, so "+
				"two nodes disagreeing about it are diverged and nothing would notice",
				name)
		}
	}
}

// TestAMissingNodeChangesTheHash is #103's own shape: the whole point is to notice
// a member map that is short one node at an unchanged generation.
func TestAMissingNodeChangesTheHash(t *testing.T) {
	full := hashTestConfig()
	short := hashTestConfig()
	delete(short.Nodes, "node-b")

	if hashOf(t, full) == hashOf(t, short) {
		t.Error("a config missing a whole node hashed the same as the complete one. " +
			"That is exactly the divergence #103 is about, and it would stay invisible")
	}
}

// TestAChangedGroupChangesTheHash covers the other divergence seen in the field:
// whitecrane's node-3 held all but the last four addresses it had missed
// (docs/TEST-PLAN.md #5).
func TestAChangedGroupChangesTheHash(t *testing.T) {
	full := hashTestConfig()
	short := hashTestConfig()
	short.Groups["group1"] = short.Groups["group1"][:1]

	if hashOf(t, full) == hashOf(t, short) {
		t.Error("a group missing an address hashed the same as the complete one")
	}
}

// TestGroupOrderDoesNotChangeTheHash keeps the detector from firing on two nodes
// that hold the same addresses in a different order. They are not diverged, and a
// pull would be pure noise -- repeated once a second, per peer.
func TestGroupOrderDoesNotChangeTheHash(t *testing.T) {
	forward := hashTestConfig()
	reversed := hashTestConfig()
	reversed.Groups["group1"] = []string{"10.0.0.2/24", "10.0.0.1/24"}
	reversed.Nodes["node-a"].IPGroups["eth0"] = []string{"group1"}

	if hashOf(t, forward) != hashOf(t, reversed) {
		t.Error("the same addresses in a different order hashed differently; the " +
			"detector would report divergence on a converged cluster")
	}
}

// TestHashingDoesNotMutateTheConfig is the safety property. sharedConfigHash sorts
// slices to get canonical ordering, and it must do that to a copy: sorting the
// config this node is running on would reorder a group's addresses under whatever
// is reading it, and #43's wholesale-apply semantics make group contents
// load-bearing.
func TestHashingDoesNotMutateTheConfig(t *testing.T) {
	cfg := hashTestConfig()
	cfg.Groups["group1"] = []string{"10.0.0.9/24", "10.0.0.1/24"}
	before := append([]string{}, cfg.Groups["group1"]...)

	hashOf(t, cfg)

	if !reflect.DeepEqual(before, cfg.Groups["group1"]) {
		t.Errorf("hashing reordered the live config's group from %v to %v",
			before, cfg.Groups["group1"])
	}
}

// TestANilConfigHashesToNothing keeps the nil case from looking like agreement.
// sharedConfigHash returns "" for nil, and the caller must treat an empty hash as
// "unknown", never as a value to compare -- two nodes that both fail to produce
// one would otherwise look converged.
func TestANilConfigHashesToNothing(t *testing.T) {
	h, err := sharedConfigHash(nil)
	if err != nil {
		t.Fatalf("hashing nil: %v", err)
	}
	if h != "" {
		t.Errorf("nil config hashed to %q, want the empty string", h)
	}
}

// TestAbsentAndEmptyHashTheSame is the false positive that a repair would have
// created for itself, and it was found by the end-to-end repair test rather than
// by reasoning.
//
// A config that has never had a plugin marshals `"plugins": null`. The same config
// after a round trip through a sync payload marshals `"plugins": {}`. So the moment
// a node finished repairing itself from the coordinator its hash disagreed with the
// coordinator's again -- and it would have pulled another repair every interval,
// forever, having already converged.
func TestAbsentAndEmptyHashTheSame(t *testing.T) {
	absent := hashTestConfig()
	absent.Plugins = nil
	empty := hashTestConfig()
	empty.Plugins = map[string]interface{}{}

	if hashOf(t, absent) != hashOf(t, empty) {
		t.Error("a nil map and an empty one hashed differently. A repair is a round " +
			"trip through JSON, so the repaired node would disagree with the " +
			"coordinator the instant it agreed with it, and pull again every interval")
	}

	// The same for the two other maps and a slice, since all four cross the wire.
	nilGroups := hashTestConfig()
	nilGroups.Groups = nil
	emptyGroups := hashTestConfig()
	emptyGroups.Groups = map[string][]string{}
	if hashOf(t, nilGroups) != hashOf(t, emptyGroups) {
		t.Error("a nil Groups map and an empty one hashed differently")
	}

	nilIPGroups := hashTestConfig()
	nilIPGroups.Nodes["node-a"].IPGroups = nil
	emptyIPGroups := hashTestConfig()
	emptyIPGroups.Nodes["node-a"].IPGroups = map[string][]string{}
	if hashOf(t, nilIPGroups) != hashOf(t, emptyIPGroups) {
		t.Error("a nil IPGroups map and an empty one hashed differently")
	}
}

// TestAHashSurvivesARoundTripThroughAPayload is the property the test above is
// really about, checked end to end through the code that actually does it: a
// config that has been through a sync payload must hash the same as the one it
// came from, or every repair leaves the node looking diverged.
func TestAHashSurvivesARoundTripThroughAPayload(t *testing.T) {
	cfg := hashTestConfig()
	cfg.Plugins = nil
	before := hashOf(t, cfg)

	payload, err := buildFullConfigPayload(cfg, nil, 1, "", "node-a",
		configStamp{version: 1, origin: "node-a"})
	if err != nil {
		t.Fatalf("buildFullConfigPayload: %v", err)
	}
	var after config.Config
	if err := json.Unmarshal(payload, &after); err != nil {
		t.Fatalf("unmarshalling the payload: %v", err)
	}

	// The receiver preserves its own identity, so match what it would hold.
	after.Pulse.LocalNode = "node-b"

	if got := hashOf(t, &after); got != before {
		t.Errorf("a config hashed %s, and %s after a round trip through a sync "+
			"payload. Every repaired node would still look diverged",
			shortHash(before), shortHash(got))
	}
}
