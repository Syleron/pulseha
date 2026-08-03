package membership

import (
	"errors"
	"testing"
)

// Regression for docs/TEST-PLAN.md defect #33, the residual half. The
// over-announcing half was fixed in 541111c by re-reading each address against
// the kernel inside the batch; this is the opposite failure, and the one that
// costs traffic rather than log lines.
//
// Run 30's evidence: 12 BringUpIP RPCs requested 559 bring-ups for a
// 288-address group, and node-1's *final* 72 were placed by the ENFORCE pass,
// which announced nothing at all. An address can therefore end up live under a
// holder that never announced it, and nothing re-announces on its own, so
// neighbours keep the previous owner's MAC until their ARP entries age out.
// That is #11/#15's risk realised: a silent partial outage that survives
// convergence.
func TestPlacingMissingAddressesAnnouncesThem(t *testing.T) {
	missing := []string{"10.0.0.1/23", "10.0.0.2/23", "10.0.0.3/23"}

	var broughtUp []string
	bringUp := func(iface, ip string) error {
		broughtUp = append(broughtUp, ip)
		return nil
	}

	var announcedOn string
	var announced [][]string
	announce := func(iface string, ips []string) ([]string, error) {
		announcedOn = iface
		announced = append(announced, ips)
		return nil, nil
	}

	attempts, skipped, err := placeMissingFloatingIPs("enX0", missing, bringUp, announce)

	if len(broughtUp) != 3 {
		t.Errorf("brought up %v, want all three missing addresses", broughtUp)
	}
	// One batch, not one announcement per address: per-IP GARP inside a
	// placement loop took four seconds an address and is what defects #4/#8
	// were.
	if len(announced) != 1 {
		t.Fatalf("announced in %d batches, want exactly 1", len(announced))
	}
	if announcedOn != "enX0" {
		t.Errorf("announced on %q, want enX0", announcedOn)
	}
	if len(announced[0]) != 3 {
		t.Errorf("announced %v, want all three addresses placed", announced[0])
	}
	if len(attempts) != 3 {
		t.Errorf("got %d attempts, want one per address", len(attempts))
	}
	for _, attempt := range attempts {
		if attempt.Err != nil {
			t.Errorf("%s: Err = %v, want nil", attempt.IP, attempt.Err)
		}
		if attempt.Iface != "enX0" {
			t.Errorf("%s: Iface = %q, want enX0", attempt.IP, attempt.Iface)
		}
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want none", skipped)
	}
	if err != nil {
		t.Errorf("unexpected announce error %v", err)
	}
}

// The announce set is what this pass *attempted*, not what it believed it
// achieved. A bring-up that reports failure for an address the kernel does in
// fact hold is documented behaviour, not a hypothetical: #45 is that race, and
// BringUpIP already carries two rechecks for it. Deciding the announce set from
// the success list is what leaves such an address live and unannounced.
//
// Handing the attempted set to the announcement is safe because the batch
// re-reads each address against the kernel immediately before its own arping
// (541111c) — it announces the ones the interface holds and reports the rest as
// skipped. So the kernel decides, at announce time, which is the whole point:
// a list built during the placement loop is stale by the time the batch reaches
// its last wave.
func TestAnAddressThatCameUpDespiteAFailedBringUpIsStillAnnounced(t *testing.T) {
	missing := []string{"10.0.0.1/23", "10.0.0.2/23"}

	wantErr := errors.New("file exists")
	bringUp := func(iface, ip string) error {
		if ip == "10.0.0.2/23" {
			return wantErr
		}
		return nil
	}

	var announced []string
	announce := func(iface string, ips []string) ([]string, error) {
		announced = ips
		return nil, nil
	}

	attempts, _, _ := placeMissingFloatingIPs("enX0", missing, bringUp, announce)

	// The address whose bring-up failed must still be offered to the
	// announcement, which is the only thing that reads kernel state late enough
	// to know whether it came up.
	found := false
	for _, ip := range announced {
		if ip == "10.0.0.2/23" {
			found = true
		}
	}
	if !found {
		t.Errorf("announced %v, want 10.0.0.2/23 offered too — its bring-up "+
			"reported failure but the kernel may hold it (#45)", announced)
	}
	// The failure is still reported: announcing it is not the same as claiming
	// the placement worked.
	if len(attempts) != 2 {
		t.Fatalf("got %d attempts, want 2", len(attempts))
	}
	if !errors.Is(attempts[1].Err, wantErr) {
		t.Errorf("10.0.0.2/23: Err = %v, want %v", attempts[1].Err, wantErr)
	}
}

// The addresses the announcement skipped are handed back so the caller can log
// them on the daemon's logger. Nothing calls SetLevel on packages/network's
// package-level logger, so a line reported there cannot reach the journal at any
// logging_level and the skip would be unverifiable live (#61's lesson). That
// line is #33's positive control: zero GARP failures alone cannot distinguish a
// working fix from a race that never occurred.
func TestPlacingReportsWhatTheAnnouncementSkipped(t *testing.T) {
	announce := func(iface string, ips []string) ([]string, error) {
		return []string{"10.0.0.2/23"}, nil
	}

	_, skipped, err := placeMissingFloatingIPs("enX0",
		[]string{"10.0.0.1/23", "10.0.0.2/23"},
		func(iface, ip string) error { return nil }, announce)

	if len(skipped) != 1 || skipped[0] != "10.0.0.2/23" {
		t.Errorf("skipped = %v, want [10.0.0.2/23]", skipped)
	}
	if err != nil {
		t.Errorf("unexpected error %v", err)
	}
}

// A failed announcement must not be reported as a failed placement: the
// addresses are up and serving either way, and a switch relearns them on the
// next ARP exchange regardless. The errors have to stay distinguishable because
// the enforce pass logs a placement failure at error level.
func TestAFailedAnnouncementIsNotAFailedPlacement(t *testing.T) {
	wantErr := errors.New("network interface does not exist")
	announce := func(iface string, ips []string) ([]string, error) {
		return nil, wantErr
	}

	attempts, _, err := placeMissingFloatingIPs("enX0", []string{"10.0.0.1/23"},
		func(iface, ip string) error { return nil }, announce)

	if !errors.Is(err, wantErr) {
		t.Errorf("announce error = %v, want %v", err, wantErr)
	}
	if len(attempts) != 1 || attempts[0].Err != nil {
		t.Errorf("attempts = %+v, want one successful placement", attempts)
	}
}

// The enforce pass reaches this with an empty set on almost every tick — the
// steady state is that nothing is missing. Announcing then would be a netlink
// read and a log line per interface per 30s for no reason, and an empty batch
// must not touch the interface at all.
func TestNothingIsAnnouncedWhenThereIsNothingToPlace(t *testing.T) {
	bringUp := func(iface, ip string) error {
		t.Error("brought up an address for an empty missing set")
		return nil
	}
	announce := func(iface string, ips []string) ([]string, error) {
		t.Error("announced for an empty missing set")
		return nil, nil
	}

	attempts, skipped, err := placeMissingFloatingIPs("enX0", nil, bringUp, announce)

	if len(attempts) != 0 || len(skipped) != 0 || err != nil {
		t.Errorf("attempts=%v skipped=%v err=%v, want all empty", attempts, skipped, err)
	}
}

// `file exists` is the specific failure worth pinning, because it means the
// address is up — another writer placed it between this pass listing it as
// missing and reaching it, which on a converging cluster is the common case, not
// the rare one (#45). A pass that announced only its own successes would stay
// silent for precisely the addresses that just changed hands, which is the
// situation an announcement exists for.
func TestAnAddressAnotherWriterPlacedFirstIsStillAnnounced(t *testing.T) {
	alreadyBack := errors.New("file exists")

	var announced []string
	announce := func(iface string, ips []string) ([]string, error) {
		announced = ips
		return nil, nil
	}

	placeMissingFloatingIPs("enX0", []string{"10.0.0.1/23"},
		func(iface, ip string) error { return alreadyBack }, announce)

	if len(announced) != 1 || announced[0] != "10.0.0.1/23" {
		t.Errorf("announced %v, want the address announced — `file exists` means "+
			"it is up, and it just changed hands", announced)
	}
}
