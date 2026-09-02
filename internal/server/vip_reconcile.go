// PulseHA - HA Cluster Daemon
// Copyright (C) 2017-2021  Andrew Zak <andrew@linux.com>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package server

import (
	"context"
	"time"

	"github.com/syleron/pulseha/packages/network"
	"github.com/syleron/pulseha/packages/pulselock"
	"github.com/syleron/pulseha/rpc"
)

// vipReconcileDelay is how long the post-load VIP reconcile waits before it
// runs. It was a bare sleep inside loadInitialMembers' goroutine, there to let
// the listeners come up; it doubles as this pass's coalescing window.
const vipReconcileDelay = 500 * time.Millisecond

// vipReconcileSnapshot is the config half of one scheduled pass, taken
// synchronously by the caller. See snapshotVIPGroups for why it cannot be read
// after the delay below.
type vipReconcileSnapshot struct {
	localID      string
	groupIPs     map[string][]string
	activeActive bool
}

// vipReconciler runs the post-load VIP reconcile with one pass in flight and at
// most one snapshot pending, the newest one winning.
//
// docs/TEST-PLAN.md defect #65, and the same multiplication #63 removed from the
// enforce path one layer over. loadInitialMembers runs on every *full*
// ConfigSync, not only at startup, and it spawned this pass unconditionally — so
// a burst of `group add-ip` calls, each of which broadcasts the config, put one
// whole-share bring-up per add on every peer that received it. Each of those
// announces through its own SendGARPBatch, whose garpFanout of 32 is per call
// and bounds nothing across calls, so run 33's 40-address burst drove concurrent
// arping to 255/268/258 on the three receiving nodes — about 8 x garpFanout —
// while the enforce pass on those same nodes ran 2-3 batches. The fourth node
// read 7, and that asymmetry is the confirmation: a node never ConfigSyncs
// itself, so the node the adds were issued on is the one this path never fires
// on.
//
// Newest-wins rather than #63's must-not-drop queue, and the difference is in
// where the snapshot comes from. TriggerEnforce has to run a queued pass because
// its callers are writes and a pass already running may have read the
// expectation set before one of them landed. Here the snapshot is taken by the
// scheduler, so a pending one is by construction newer than the pass in flight
// and describes the config the node should converge on now; an older snapshot it
// replaces can only be a superseded view of the same thing. Nothing is lost,
// because a pass always begins strictly after the newest snapshot it will act on
// was taken.
type vipReconciler struct {
	mu      pulselock.Mutex
	running bool
	pending *vipReconcileSnapshot

	delay time.Duration
	// run is the pass itself, and sleep is the window before it. Both are fields
	// so a test can drive the coalescing deterministically instead of by waiting.
	run   func(vipReconcileSnapshot)
	sleep func(time.Duration)
}

func newVIPReconciler(delay time.Duration, run func(vipReconcileSnapshot)) *vipReconciler {
	return &vipReconciler{delay: delay, run: run, sleep: time.Sleep}
}

// Schedule records a snapshot to reconcile against and returns immediately,
// starting a pass only if one is not already under way.
func (r *vipReconciler) Schedule(snapshot vipReconcileSnapshot) {
	if r == nil {
		return
	}

	r.mu.Lock()
	r.pending = &snapshot
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.mu.Unlock()

	go r.loop()
}

// loop waits out the window, runs the newest snapshot, and repeats for whatever
// arrived while it was busy.
//
// The window is waited *before* the snapshot is taken, which is what makes the
// coalescing work: every sync that lands during it collapses into the one pass
// that follows.
func (r *vipReconciler) loop() {
	for {
		r.sleep(r.delay)

		r.mu.Lock()
		snapshot := r.pending
		r.pending = nil
		if snapshot == nil {
			r.running = false
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()

		r.run(*snapshot)
	}
}

// vipReconcileQueue returns the reconciler, creating it on first use — the same
// reason peerBringUpQueue is lazy: Server is built as a struct literal in
// several places, tests included, and none of them would set this field.
func (s *Server) vipReconcileQueue() *vipReconciler {
	s.vipReconcileMu.Lock()
	defer s.vipReconcileMu.Unlock()
	if s.vipReconcile == nil {
		s.vipReconcile = newVIPReconciler(vipReconcileDelay, s.runVIPReconcile)
	}
	return s.vipReconcile
}

// vipReconcileTargets reduces a plan to the addresses the pass should actually
// act on, which for a claim means the ones the interface is not already holding
// and for a release means all of them.
//
// The claim half is the other half of defect #65, and the larger one: the
// frequency above is worth about 8x, this is worth the size of the share. The
// claim plan is everything the node should hold, so on a converged node it is the
// node's entire assignment — 62 to 72 addresses on the whitecrane topology — and
// every one of them was handed to BringUpIP, which deliberately announces every
// address it is *asked* about rather than the ones it placed (#33's residual
// half: the kernel decides at announce time). That rule is right, and it makes
// the caller responsible for asking only about addresses that need placing. Every
// sibling caller already does — refreshLocalMonitorExpectedIPs since #64, and the
// ENFORCE pass' Active branch — both computing a missing set from one snapshot
// first. This was the one whole-share caller left, so an add of one address
// re-announced the other 71.
//
// The release half is deliberately left whole-group and must stay that way: a
// node that has just been demoted may be holding addresses it was never
// assigned, and the point of that direction is to leave it holding none.
// BringDownIP has filtered the request against its own inventory snapshot since
// #34, so nothing is released twice either.
//
// A nil lookup means kernel state could not be read and every address is
// reported missing, which is missingOnIface's contract and the same direction
// every other filter on this path takes: a check that cannot see must not turn a
// placement into a silent no-op.
//
// Unparseable addresses come back separately so the caller can say so. They are
// skipped either way — an address that cannot be parsed cannot be placed — but
// silently is the wrong way to skip a configured group entry (see missingOnIface).
func vipReconcileTargets(plan map[string][]string, claim bool,
	heldOn func(ip string) (bool, string)) (map[string][]string, []string) {

	if !claim {
		return plan, nil
	}

	var invalid []string
	narrowed := make(map[string][]string, len(plan))
	for iface, ips := range plan {
		missing, bad := missingOnIface(iface, ips, heldOn)
		invalid = append(invalid, bad...)
		if len(missing) > 0 {
			narrowed[iface] = missing
		}
	}
	return narrowed, invalid
}

// runVIPReconcile is the pass: decide what the local node should hold now, and
// place or release accordingly.
func (s *Server) runVIPReconcile(snapshot vipReconcileSnapshot) {
	plan, claim := s.reconcileVIPPlan(snapshot.localID, snapshot.groupIPs, snapshot.activeActive)
	if len(plan) == 0 {
		return
	}

	// One snapshot for the whole pass rather than a netlink dump per address:
	// network.CheckIfIPExists builds a complete inventory, every link and both
	// families, on each call (#64). Skipped entirely for a release, which acts on
	// the whole group regardless of what the snapshot would say.
	var heldOn func(ip string) (bool, string)
	if claim {
		if inventory, err := network.BuildIPInventory(); err != nil {
			s.logger.Warn("VIP_RECONCILE: could not read interface addresses; treating every claimed address as missing",
				"error", err)
		} else {
			heldOn = ipInventoryLookup(inventory)
		}
	}

	targets, invalid := vipReconcileTargets(plan, claim, heldOn)
	if len(invalid) > 0 {
		// A configured group entry that cannot be parsed. Warn rather than skip
		// quietly: the address simply never gets placed, which looks like a
		// floating IP that will not come up rather than a config typo.
		s.logger.Warn("VIP_RECONCILE: skipping unparseable configured addresses; "+
			"these will never be placed until the config is corrected",
			"addresses", invalid)
	}
	if len(targets) == 0 {
		s.logger.Debug("VIP_RECONCILE: every claimed address is already held; nothing to place or announce")
		return
	}

	for iface, ips := range targets {
		if claim {
			_, _ = s.BringUpIP(context.Background(), &rpc.UpIpRequest{Iface: iface, Ips: ips})
		} else {
			_, _ = s.BringDownIP(context.Background(), &rpc.DownIpRequest{Iface: iface, Ips: ips})
		}
	}
}
