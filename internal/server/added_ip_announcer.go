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
	"slices"

	"github.com/syleron/pulseha/packages/pulselock"
)

// addedIPAnnouncer sends AddIPToGroup's announcements (GitHub #289) from one
// goroutine, so a burst of adds never has more than one announcement batch
// running.
//
// One goroutine per add would not be bounded. Each SendGARPBatch call limits
// its own fan-out to garpFanout, but separate calls do not share that limit.
// The appliance's reconcile adds hundreds of addresses back to back at about
// 38ms per add (docs/TEST-PLAN.md #37), and each arping lives about four
// seconds. In active-passive every one of those adds lands on this node, so
// about a hundred arping processes would run at once.
//
// A fixed window, as peerBringUpBatcher uses, would not bound it either:
// batches from successive windows still overlap while each one's arpings run.
// What bounds it is the single drain loop. Addresses added while a batch is
// being announced wait and go out together in the next batch, so concurrency
// stays at one SendGARPBatch call, at most garpFanout arpings. An isolated add
// is announced at once, with no window to wait out.
type addedIPAnnouncer struct {
	mu pulselock.Mutex
	// send announces one interface's addresses. It is called from the drain
	// goroutine only, one call at a time.
	send    func(iface string, ips []string)
	pending map[string][]string
	running bool
}

func newAddedIPAnnouncer(send func(iface string, ips []string)) *addedIPAnnouncer {
	return &addedIPAnnouncer{send: send, pending: make(map[string][]string)}
}

// Add queues ip for announcement on iface and returns immediately. It starts
// the drain goroutine if one is not already running. An address already
// waiting on the same interface is not queued twice.
func (a *addedIPAnnouncer) Add(iface, ip string) {
	a.mu.Lock()
	if !slices.Contains(a.pending[iface], ip) {
		a.pending[iface] = append(a.pending[iface], ip)
	}
	start := !a.running
	a.running = true
	a.mu.Unlock()

	if start {
		go a.drain()
	}
}

// drain announces whatever is pending, one interface at a time, until nothing
// is left. It takes the whole pending set at once, so everything queued while
// the previous batch ran goes out in one call per interface. It clears
// running under the same lock that finds the set empty, so an Add racing the
// exit either lands in a batch this loop sends or starts a new loop.
func (a *addedIPAnnouncer) drain() {
	for {
		a.mu.Lock()
		if len(a.pending) == 0 {
			a.running = false
			a.mu.Unlock()
			return
		}
		batch := a.pending
		a.pending = make(map[string][]string)
		a.mu.Unlock()

		ifaces := make([]string, 0, len(batch))
		for iface := range batch {
			ifaces = append(ifaces, iface)
		}
		slices.Sort(ifaces)
		for _, iface := range ifaces {
			a.send(iface, batch[iface])
		}
	}
}
