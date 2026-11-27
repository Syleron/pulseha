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

package network

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// duplicateProbeWait is how long arping listens for an answer, in whole seconds
// because that is the unit its -w takes.
//
// This is the price of the check and it is paid on every address that is new to
// the configuration, so it is as short as arping allows. A host on the same
// segment answers an ARP probe in well under a millisecond; one second is there
// for the probe that is lost, not for a slow reply.
const duplicateProbeWait = 1

// duplicateProbeTimeout caps the arping process itself. The probe is expected to
// take duplicateProbeWait; this only fires on a process that is stuck, and a
// stuck probe is reported as "could not tell", never as "in use".
const duplicateProbeTimeout = 5 * time.Second

// arpReplyMAC picks the answering MAC out of arping's reply line, which reads
// "Unicast reply from 10.0.0.1 [02:42:AC:11:00:02]  0.5ms".
var arpReplyMAC = regexp.MustCompile(`\[([0-9A-Fa-f]{2}(?::[0-9A-Fa-f]{2}){5})\]`)

// duplicateProbeCommand returns the argv that asks iface's segment whether any
// host answers for ip, and probe=false when there is nothing to ask.
//
// IPv4 only. The kernel runs duplicate address detection itself when an IPv6
// address is added and refuses to use one that fails, so IPv6 has this check
// already and ARP could not perform it anyway.
//
// `-D` is the RFC 5227 probe: the sender address is 0.0.0.0, so it works on an
// interface that holds no address in the subnet, which is the usual state of an
// interface that only carries floating IPs, and it does not disturb any
// neighbour's ARP cache the way an ordinary request would. `-f` stops at the
// first answer, so an address that is in use is reported without waiting out
// the deadline.
//
// Kept pure, like announceCommand, so the argv is testable without a network.
func duplicateProbeCommand(iface, ip string) (name string, args []string, probe bool, err error) {
	target, err := parseTargetIP(ip)
	if err != nil {
		return "", nil, false, err
	}
	if target == nil {
		return "", nil, false, errors.New("invalid IP address: " + ip)
	}
	if target.To4() == nil {
		return "", nil, false, nil
	}
	return "arping", []string{
		"-D", "-f", "-c", "2", "-w", fmt.Sprint(duplicateProbeWait), "-I", iface, target.String(),
	}, true, nil
}

// interpretDuplicateProbe turns arping -D's result into an answer.
//
// arping -D exits 0 when nothing answered and 1 when something did. Anything
// else, including an exit of 1 with no reply in the output, is a probe that did
// not run properly: the interface is down, the binary refused its arguments, the
// process was killed. Those come back as an error so that the caller can tell
// "in use" from "could not tell" and never refuses an address on the strength of
// a probe that failed.
func interpretDuplicateProbe(output string, exitCode int, runErr error) (holder string, held bool, err error) {
	if runErr == nil {
		return "", false, nil
	}
	if exitCode == 1 {
		if match := arpReplyMAC.FindStringSubmatch(output); match != nil {
			return strings.ToLower(match[1]), true, nil
		}
	}
	return "", false, fmt.Errorf("duplicate address probe failed: %w (output: %q)", runErr, strings.TrimSpace(output))
}

// ProbeAddress asks iface's segment whether any host answers for ip. holder is
// the MAC that answered.
//
// It sends and listens on the wire and nothing else: it does not consult this
// node's own addresses. AddressHeldElsewhere is the one to call for the question
// "is this address somebody else's".
func ProbeAddress(iface, ip string) (holder string, held bool, err error) {
	name, args, probe, err := duplicateProbeCommand(iface, ip)
	if err != nil || !probe {
		return "", false, err
	}
	if _, err := requireAnnouncer(name); err != nil {
		return "", false, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), duplicateProbeTimeout)
	defer cancel()
	// Not utils.ExecuteWithTimeout: that drops the output on a non-zero exit, and
	// a non-zero exit is where the answer is.
	output, runErr := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", false, fmt.Errorf("duplicate address probe timed out after %s", duplicateProbeTimeout)
	}

	exitCode := 0
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		exitCode = exitErr.ExitCode()
	}
	return interpretDuplicateProbe(string(output), exitCode, runErr)
}

// AddressHeldElsewhere reports whether a host other than this node answers for
// ip on iface's segment, and the MAC that answered.
//
// It exists for the moment an address is first configured as a floating IP.
// Bringing an address up announces it, and the announcement rewrites every
// neighbour's ARP entry, so configuring an address that already belongs to
// another machine takes that machine off the network: its traffic arrives here
// instead. Removing the address afterwards does not undo it, because nothing
// re-announces on the real owner's behalf and neighbours keep this node's MAC
// until their entries age out.
//
// Not for failover. A node taking over an address expects its previous holder to
// be answering for it until the announcement lands, and a probe there would
// delay or refuse the very transfer the cluster exists to make.
//
// held=false with a nil error also covers the cases where there is nothing to
// ask: an IPv6 address, an interface that does not exist, an address this node
// already holds.
func AddressHeldElsewhere(iface, ip string) (holder string, held bool, err error) {
	if exists, _ := InterfaceExist(iface); !exists {
		return "", false, nil
	}
	// An address already on this node is ours whatever the wire says, and the
	// kernel would not answer its own probe for it.
	if exists, _, err := CheckIfIPExists(ip); err != nil {
		return "", false, err
	} else if exists {
		return "", false, nil
	}
	return ProbeAddress(iface, ip)
}
