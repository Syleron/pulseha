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
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/packages/pulselock"
	"github.com/syleron/pulseha/packages/utils"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

type ICMPv6MessageHeader struct {
	Type     byte
	Code     byte
	Checksum uint16
}

type ICMPv6NeighborSolicitation struct {
	Header            ICMPv6MessageHeader
	Reserved          uint32
	TargetAddress     [16]byte
	OptionType        byte
	OptionLength      byte
	SourceLinkAddress [6]byte
}

// IPInventory captures a snapshot of IP assignments across interfaces so that callers can
// make multiple existence checks without repeatedly walking netlink state.
type IPInventory struct {
	ipToIface map[string]string
}

// BuildIPInventory builds a snapshot of IP assignments using a single netlink handle.
func BuildIPInventory() (*IPInventory, error) {
	handle, err := netlink.NewHandle()
	if err != nil {
		log.Debug("NETWORK: BuildIPInventory failed to create netlink handle", "error", err)
		return nil, err
	}
	defer handle.Delete()

	links, err := handle.LinkList()
	if err != nil {
		log.Debug("NETWORK: BuildIPInventory failed to list links", "error", err)
		return nil, err
	}

	ipMap := make(map[string]string)
	for _, link := range links {
		if link == nil || link.Attrs() == nil {
			continue
		}
		iface := link.Attrs().Name
		for _, family := range []int{unix.AF_INET, unix.AF_INET6} {
			addrs, err := handle.AddrList(link, family)
			if err != nil {
				log.Debug("NETWORK: BuildIPInventory failed to list addresses", "iface", iface, "family", family, "error", err)
				continue
			}
			for _, addr := range addrs {
				normalized, ok := normalizeIP(addr.IP)
				if !ok {
					continue
				}
				ipMap[ipKey(normalized)] = iface
			}
		}
	}

	return &IPInventory{ipToIface: ipMap}, nil
}

// Exists checks whether the provided IP (string or CIDR) is present in the inventory and
// returns the interface if found.
func (inv *IPInventory) Exists(ipAddr string) (bool, string, error) {
	targetIP, err := parseTargetIP(ipAddr)
	if err != nil {
		return false, "", err
	}
	if targetIP == nil {
		return false, "", errors.New("invalid IP address: " + ipAddr)
	}

	iface, ok := inv.ipToIface[ipKey(targetIP)]
	if !ok {
		return false, "", nil
	}
	return true, iface, nil
}

func normalizeIP(ip net.IP) (net.IP, bool) {
	if ip == nil {
		return nil, false
	}
	if v4 := ip.To4(); v4 != nil {
		return net.IP(v4), true
	}
	v6 := ip.To16()
	if v6 == nil {
		return nil, false
	}
	// Guard against v4-mapped IPv6 addresses
	if v4mapped := v6.To4(); v4mapped != nil {
		return net.IP(v4mapped), true
	}
	return net.IP(v6), true
}

func ipKey(ip net.IP) string {
	if ip == nil {
		return ""
	}
	if len(ip) == net.IPv4len {
		return "4|" + net.IP(ip).String()
	}
	return "6|" + net.IP(ip).String()
}

// parseTargetIP normalises an address or CIDR to a single net.IP.
//
// Shared by the inventory lookup and by announceCommand, which is why its log lines
// no longer name CheckIfIPExists: they were written when this was that function's
// body, and after #66 gave the announce path its own family decision the same lines
// were attributing announce failures to a liveness check that had not run.
func parseTargetIP(ipAddr string) (net.IP, error) {
	log.Debug("parseTargetIP called", "searchIP", ipAddr)

	if strings.Contains(ipAddr, "/") {
		parsedIP, _, err := net.ParseCIDR(ipAddr)
		if err != nil {
			log.Debug("parseTargetIP invalid CIDR", "input", ipAddr, "error", err)
			return nil, err
		}
		if normalized, ok := normalizeIP(parsedIP); ok {
			return normalized, nil
		}
		log.Debug("parseTargetIP unsupported IP family", "input", ipAddr)
		return nil, errors.New("unsupported IP address family for: " + ipAddr)
	}

	parsedIP := net.ParseIP(ipAddr)
	if parsedIP == nil {
		log.Debug("parseTargetIP invalid IP", "input", ipAddr)
		return nil, errors.New("invalid IP address: " + ipAddr)
	}
	if normalized, ok := normalizeIP(parsedIP); ok {
		return normalized, nil
	}
	log.Debug("parseTargetIP unsupported IP family", "input", ipAddr)
	return nil, errors.New("unsupported IP address family for: " + ipAddr)
}

// announceCommand returns the command that tells the segment this interface now
// answers for ip: gratuitous ARP for IPv4, an unsolicited neighbour advertisement
// for IPv6.
//
// Defect #66. Announcing was ARP-only — `arping -U` for every address whatever its
// family — which on an IPv6-only cluster means no floating IP is ever announced and
// neighbours keep the previous owner's MAC until their NDP cache ages out. Worse
// than silent: `arping -U` against a v6 address exits **2**, the same code #33
// reads as "the interface does not hold this address", so the one channel that
// defect is scored on reports a constant instead of a signal.
//
// A v4-mapped address is an IPv4 address in v6 clothing and ARP still answers for
// it, so the family line is drawn with normalizeIP — the same line BuildIPInventory
// draws, because an announcement that disagreed with the inventory would announce
// on one family and be liveness-checked on the other.
//
// Split out from SendGARP and kept pure so the argv is testable without a network,
// a Linux host, or either binary installed: what #66 got wrong was the argv, not
// the fan-out around it. NOTE: assumes Linux with "arping" and "ndptool" installed.
func announceCommand(iface, ip string) (string, []string, error) {
	target, err := parseTargetIP(ip)
	if err != nil {
		return "", nil, err
	}
	if target == nil {
		return "", nil, errors.New("invalid IP address: " + ip)
	}

	if target.To4() != nil {
		return "arping", []string{"-U", "-c", "5", "-I", iface, target.String()}, nil
	}
	// `send` is a subcommand and -T is the ICMPv6 target: ndptool with either
	// missing sends nothing. The tree carried a dead IPv6NDP(iface) helper with
	// neither, which is why this looked handled and was not.
	return "ndptool", []string{"-t", "na", "-U", "-i", iface, "-T", target.String(), "send"}, nil
}

// announcerPaths caches the PATH lookup per announcer binary.
//
// Cached rather than probed per call because a whole-group announce runs this once
// an address — 288 on the largest whitecrane topology — and the answer cannot change
// within a daemon's lifetime in any way worth tracking.
var announcerPaths sync.Map // binary name -> announcerLookup

type announcerLookup struct {
	path string
	err  error
}

// requireAnnouncer resolves an announcer binary on PATH and says plainly what is
// missing when it is not there.
//
// The two announcers are not interchangeable and neither is guaranteed present:
// arping and ndptool ship in separate packages, and ndptool's `-U` is a libndp
// addition rather than something every release carries. Without this the failure
// arrived as a bare exec error once per address, which on an IPv6-only cluster is
// one line per floating IP saying nothing about the cause.
//
// Verified working on the target platform rather than assumed: run 34 (2026-08-07)
// captured four unsolicited NAs on the wire from this exact argv on an IPv6-only
// whitecrane, so `-U` and the binary are both present there. This guard is for the
// hosts that are not that one.
func requireAnnouncer(name string) (string, error) {
	if cached, ok := announcerPaths.Load(name); ok {
		lookup := cached.(announcerLookup)
		return lookup.path, lookup.err
	}

	path, err := exec.LookPath(name)
	if err != nil {
		err = fmt.Errorf("announcer %q not found on PATH: %w (install it: arping "+
			"announces IPv4 floating IPs, ndptool announces IPv6 ones)", name, err)
	}
	announcerPaths.Store(name, announcerLookup{path: path, err: err})
	return path, err
}

/*
*
Announce a floating IP so the segment learns which node now answers for it —
gratuitous ARP for IPv4, an unsolicited neighbour advertisement for IPv6.
NOTE: This function assumes the OS is LINUX with "arping"/"ndptool" installed.
*/
func SendGARP(iface, ip string) error {
	exists, _ := InterfaceExist(iface)
	if !exists {
		log.Error("Unable to announce as the network interface does not exist")
		return errors.New("network interface does not exist")
	}
	name, args, err := announceCommand(iface, ip)
	if err != nil {
		log.Error("failed to announce. Cannot parse IP address", "value", ip, "error", err)
		return err
	}
	if _, err := requireAnnouncer(name); err != nil {
		log.Error("failed to announce", "ip", ip, "iface", iface, "error", err)
		return err
	}
	log.Debug("Announcing floating IP", "ip", ip, "iface", iface, "via", name)
	if _, err := utils.ExecuteWithTimeout(garpTimeout, name, args...); err != nil {
		log.Error("failed to announce. "+err.Error(), "ip", ip, "via", name)
		return err
	}
	return nil
}

// garpFanout bounds how many arping processes announce at once.
//
// Each SendGARP blocks for roughly four seconds — arping paces five packets a
// second apart — so announcing a large floating IP group one address at a time
// takes minutes. The processes spend that time asleep rather than on CPU, so the
// bound exists only to cap process and socket count, not to ration work.
const garpFanout = 32

// garpTimeout caps a single arping.
//
// The batch below ends in an unconditional wg.Wait(), so without a deadline one
// arping that never exits holds the bring-up RPC — and the failover waiting on
// it — open forever. Five packets a second apart is ~4s of expected work, so 10s
// only fires on a process that is genuinely stuck. The address is up either way:
// a killed announcement is a logged warning, not a failed bring-up.
const garpTimeout = 10 * time.Second

// SendGARPBatch announces every ip on iface, up to garpFanout at a time.
//
// A group of 200 addresses announced serially blocks the caller for over ten
// minutes. That is long enough for peers to stop seeing the node as Active and
// elect a replacement while it is still holding every address — the origin of the
// TC-6 split-brain (docs/TEST-PLAN.md defects #4/#8). Announcing concurrently
// brings the same group down to seconds.
//
// Announcement is advisory: the addresses are already up and serving before this
// runs, and a switch relearns them on the next ARP exchange regardless. So a
// failure is reported for logging but never means the address is not up.
//
// The set is announced as it stood when the caller built it, and it takes waves
// of seconds to work through, so an address can be released before its turn comes
// — the caller's list is intent, not inventory. Addresses that have gone are
// skipped and returned, not announced and not reported failed: see
// addressAbsentFrom (docs/TEST-PLAN.md defect #33).
func SendGARPBatch(iface string, ips []string) (skipped []string, err error) {
	if len(ips) == 0 {
		return nil, nil
	}
	if exists, _ := InterfaceExist(iface); !exists {
		log.Error("Unable to GARP as the network interface does not exist", "iface", iface)
		return nil, errors.New("network interface does not exist")
	}
	return sendGARPBatch(iface, ips, SendGARP, addressAbsentFrom)
}

// announceFunc announces a single address on an interface.
type announceFunc func(iface, ip string) error

// absentFunc reports that iface definitely no longer holds ip.
type absentFunc func(iface, ip string) bool

// addressAbsentFrom answers whether iface has stopped holding ip — a definite
// negative only. Callers pass CIDR form, which CheckIfIPExists accepts as it
// stands.
//
// An address whose state cannot be read answers false, so a netlink hiccup makes
// it be announced rather than silently dropped. Suppressing an announcement for
// an address that is in fact up is the one way this fix could do harm: nothing
// re-announces on its own, so neighbours would keep pointing at the previous
// owner until their ARP entries age out. Announcing one address too many costs a
// log line; announcing one too few costs traffic.
func addressAbsentFrom(iface, ip string) bool {
	exists, on, err := CheckIfIPExists(ip)
	if err != nil {
		return false
	}
	return !exists || on != iface
}

// sendGARPBatch is the fan-out half of SendGARPBatch, with the announcement and
// the liveness check injected. The real ones exec arping and read netlink, so
// this is where the concurrency bound, the skipping and the failure reporting can
// be tested without a network or a Linux host.
//
// The check runs inside the goroutine, immediately before the announcement it
// guards, rather than filtering the set up front. A pre-filter would be read once
// and then acted on for as long as the batch takes — at 32 at a time and ~4s an
// arping, the last wave of a 200-address group announces roughly half a minute
// after such a filter ran, which is most of the window the defect lives in. The
// window cannot be closed, only narrowed to the syscall, the same reasoning
// AddrAddSatisfied records for #45.
func sendGARPBatch(iface string, ips []string, announce announceFunc, absent absentFunc) (skipped []string, err error) {
	sem := make(chan struct{}, garpFanout)
	var wg sync.WaitGroup
	var mu pulselock.Mutex
	var failed []string

	for _, ip := range ips {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if absent != nil && absent(iface, ip) {
				// Released between the caller listing it and its turn here. Not a
				// failure: arping -U exits 2 on an address the interface does not
				// have, which is 173 error lines for correct behaviour (#33).
				mu.Lock()
				skipped = append(skipped, ip)
				mu.Unlock()
				return
			}

			if err := announce(iface, ip); err != nil {
				mu.Lock()
				failed = append(failed, ip)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(failed) > 0 {
		return skipped, fmt.Errorf("failed to announce %d of %d address(es) on %s (e.g. %s)",
			len(failed), len(ips), iface, strings.Join(failed[:min(3, len(failed))], ", "))
	}
	return skipped, nil
}

// AnnounceBatchTimeout is the longest SendGARPBatch can take for ipCount
// addresses: ceil(ipCount/garpFanout) waves, each bounded by garpTimeout.
//
// Exported because a bring-up ends in one of these batches, and its arping waves
// — not the netlink adds, which are sub-millisecond — are what a caller's
// deadline on that bring-up actually has to cover. Sizing such a deadline from
// the address count alone is defect #57: a flat 5s could not cover even a single
// wave, so a 24-address batch that was up almost immediately was still reported
// failed. This is an upper bound, not an estimate; a wave usually costs the ~4s
// arping spends pacing its five packets, not the 10s cap.
func AnnounceBatchTimeout(ipCount int) time.Duration {
	if ipCount <= 0 {
		return 0
	}
	waves := (ipCount + garpFanout - 1) / garpFanout
	return time.Duration(waves) * garpTimeout
}

/*
*
Checks to see what status a network interface is currently.
Possible responses are either up or down.
*/
func netInterfaceStatus(iface string) bool {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		log.Debug("netInterfaceStatus: unable to resolve interface", "iface", iface, "error", err)
		return false
	}
	attrs := link.Attrs()
	if attrs == nil {
		return false
	}
	return attrs.OperState == netlink.OperUp
}

// AddrAddSatisfied reports whether a failed address add left the interface in
// the state the caller wanted, so the failure is a no-op rather than a fault.
//
// docs/TEST-PLAN.md defect #45, the mirror of #41 on the bring-up path. Adding
// an address that is already there fails with EEXIST — `file exists` — and that
// was logged at error level and returned as a failure, which the enforce loop
// re-reported and `OrchestrateIPFailover` escalated into
// `IP_FAILOVER: Some interfaces failed to bring up IPs`: a failover reported as
// broken because an address it wanted up was up already.
//
// Two ways to reach the wanted state without this call being the one that made
// it. EEXIST is the kernel saying so directly — it only fires when this exact
// address and prefix are already on this exact link, which is the goal. Any
// other failure needs asking, so heldByTarget is consulted; like #41's
// post-failure re-check it must be a live check, because its whole purpose is
// to be newer than whatever decided to make the call. The window between a
// pre-check and the syscall cannot be closed — several writers add addresses
// here (the enforce loop, the netlink watcher's restore, the BringUpIP RPC's
// per-interface goroutines) — so it is classified instead.
//
// A failure on an address that is genuinely not up is still a failure. That is
// the line worth reading, and the noise was what would have hidden it.
func AddrAddSatisfied(addErr error, heldByTarget func() bool) bool {
	if addErr == nil {
		return true
	}
	// errors.Is against os.ErrExist rather than a bare EEXIST comparison:
	// netlink returns a syscall.Errno, sometimes wrapped with the kernel's
	// extended-ack message, and syscall.Errno/unix.Errno are distinct types
	// that both answer to os.ErrExist.
	if errors.Is(addErr, os.ErrExist) {
		return true
	}
	return heldByTarget != nil && heldByTarget()
}

/*
*
This function is to bring up a network interface
*/
func BringIPup(iface, ip string) error {
	log.Info("NETWORK: Starting BringIPup", "iface", iface, "ip", ip)
	exists, link := InterfaceExist(iface)
	if !exists {
		log.Error("NETWORK: Interface does not exist", "iface", iface)
		return errors.New("unable to bring IP up as the network interface does not exist")
	}
	log.Debug("NETWORK: Interface exists", "iface", iface)

	// Check to see if the ip already exists
	ipOb, ipNet := utils.GetCIDR(ip)
	log.Debug("NETWORK: GetCIDR result", "inputIP", ip, "ipOnly", ipOb, "ipNet", ipNet)
	if ipOb == nil {
		log.Error("NETWORK: GetCIDR returned nil IP for input", "ip", ip)
		return errors.New("invalid IP address format")
	}

	exists, eIface, err := CheckIfIPExists(ipOb.String())
	if err != nil {
		log.Debug("NETWORK: Failed to check if IP exists", "error", err)
		return err
	}
	log.Info("NETWORK: IP existence check", "ip", ipOb.String(), "exists", exists, "existingIface", eIface, "targetIface", iface)

	if exists {
		// If IP already exists on the target interface, we're done
		if eIface == iface {
			log.Info("NETWORK: IP already exists on target interface (nothing to do)", "ip", ipOb.String(), "iface", iface)
			return nil
		}
		// If IP exists on another interface, bring it down first
		log.Info("NETWORK: IP exists on different interface, removing first", "ip", ipOb.String(), "currentIface", eIface, "targetIface", iface)
		if err := BringIPdown(eIface, ip); err != nil {
			log.Warn("NETWORK: Failed to remove IP from existing interface", "ip", ipOb.String(), "iface", eIface, "error", err)
		} else {
			log.Info("NETWORK: Successfully removed IP from existing interface", "ip", ipOb.String(), "iface", eIface)
		}
	}

	addr, err := netlink.ParseAddr(ip)
	if err != nil {
		log.Error("NETWORK: Failed to parse address", "ip", ip, "error", err)
		return errors.New("unable to bring IP up because ip address couldn't be parsed")
	}
	log.Debug("NETWORK: Parsed address successfully", "ip", ip)

	log.Info("NETWORK: Adding IP to interface", "ip", ip, "iface", iface)
	if err := netlink.AddrAdd(link, addr); err != nil {
		if AddrAddSatisfied(err, func() bool {
			ex, eIface, cerr := CheckIfIPExists(ipOb.String())
			return cerr == nil && ex && eIface == iface
		}) {
			// Nothing to do and nothing wrong: the address arrived before this
			// call reached the kernel, which is the state it was asking for
			// (docs/TEST-PLAN.md defect #45).
			log.Debug("NETWORK: IP was already up when adding it", "ip", ip, "iface", iface, "error", err)
			return nil
		}
		log.Error("NETWORK: netlink.AddrAdd failed", "error", err, "ip", ip, "iface", iface)
		return errors.New("unable to bring IP up as netlink failed to do so")
	}
	log.Info("NETWORK: Successfully brought up IP", "ip", ip, "iface", iface)
	return nil
}

// AddrDelSatisfied reports whether a failed address delete left the interface in
// the state the caller wanted, so the failure is a no-op rather than a fault.
//
// docs/TEST-PLAN.md defect #61, the same shape as #45 on the release path.
// Deleting an address that is not there fails with EADDRNOTAVAIL — `cannot
// assign requested address` — and every caller of BringIPdown reported that as a
// failure. Two release paths run concurrently on a node whose group is being
// deleted: the enforce loop's surplus pass, which #41 taught to classify this,
// and the BringDownIP RPC, which had no classification at all. The RPC loses the
// race routinely and logged one error per address for work the other path had
// just completed.
//
// The inversion against AddrAddSatisfied is the point: for a delete the wanted
// state is the address being *absent*, so heldByTarget answering false is
// success. Like #41's post-failure re-check it must be a live check, because its
// purpose is to be newer than whatever decided to make the call; the window
// between a pre-check and the syscall cannot be closed, only classified.
//
// An address still up after a failed delete is still a failure. That is the line
// worth reading, and the noise was what would have hidden it.
func AddrDelSatisfied(delErr error, heldByTarget func() bool) bool {
	if delErr == nil {
		return true
	}
	// errors.Is rather than a bare comparison, for the reason given on
	// AddrAddSatisfied: netlink may wrap the errno with the kernel's extended-ack
	// message. unix.EADDRNOTAVAIL is itself a syscall.Errno, so this matches an
	// errno raised as either type.
	if errors.Is(delErr, unix.EADDRNOTAVAIL) {
		return true
	}
	return heldByTarget != nil && !heldByTarget()
}

/*
*
This function is to bring down a network interface
*/
func BringIPdown(iface, ip string) error {
	_, err := BringIPdownClassified(iface, ip)
	return err
}

// BringIPdownClassified brings an address down and reports whether it was
// already gone, so a caller with a logger the operator can actually turn up can
// say which of the two happened.
//
// The distinction exists only for observability, and it has to be surfaced here
// because this package logs through charmbracelet's package-level logger, whose
// level nothing sets — it stays at Info for the life of the process, so a Debug
// line from this file never reaches the journal at any `logging_level`. Run 29
// hit that directly: the #61 fix silenced the false error, and there was no way
// to show from the logs that the classification had fired rather than the race
// simply not happening. An all-zeros pass with no positive control is not a pass.
func BringIPdownClassified(iface, ip string) (alreadyGone bool, err error) {
	exists, link := InterfaceExist(iface)
	if !exists {
		return false, errors.New("unable to bring IP down as the network interface does not exist")
	}
	addr, parseErr := netlink.ParseAddr(ip)
	if parseErr != nil {
		return false, errors.New("unable to bring IP down because ip address couldn't be parsed")
	}
	if delErr := netlink.AddrDel(link, addr); delErr != nil {
		if AddrDelSatisfied(delErr, func() bool {
			ipOb, _ := utils.GetCIDR(ip)
			if ipOb == nil {
				// Nothing to ask with; leave the failure as a failure.
				return true
			}
			ex, eIface, cerr := CheckIfIPExists(ipOb.String())
			return cerr == nil && ex && eIface == iface
		}) {
			// Nothing to do and nothing wrong: the address had already gone,
			// which is the state this call was asking for (defect #61).
			return true, nil
		}
		log.Warn("NETWORK: Unable to bring down IP", "ip", ip, "iface", iface, "error", delErr)
		return false, errors.New("unable to bring down ip " + ip + " on interface " + iface + ": " + delErr.Error())
	}
	return false, nil
}

/*
*
Perform a curl request to a web host.
This only returns a boolean based off the http status code received by the request.
*/
func Curl(httpRequestURL string) bool {
	output, err := utils.Execute("curl", "-s", "-o", "/dev/null", "-w", "\"%{http_code}\"", httpRequestURL)
	if err != nil {
		//log.Error("Http Curl request failed.")
		return false
	}
	if output == "\"200\"" {
		return true
	} else {
		return false
	}
}

/**
 * Performs an ICMP ping to check if a host is reachable
 * Handles both plain IPs and CIDR notation
 */
func ICMPv4(Ipv4Addr string) error {
	// If the IP is in CIDR notation, extract just the IP part
	if strings.Contains(Ipv4Addr, "/") {
		ipPart, _, err := net.ParseCIDR(Ipv4Addr)
		if err != nil {
			log.Error("Failed to parse CIDR address: ", Ipv4Addr)
			return err
		}
		Ipv4Addr = ipPart.String()
	}

	cmds := "ping -c 1 -W 1 " + Ipv4Addr + " &> /dev/null ; echo $?"
	cmd := exec.Command("bash", "-c", cmds)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		log.Error("ICMP request failed: ", Ipv4Addr)
		return err
	}
	if !strings.Contains(out.String(), "0") {
		log.Error("ICMP request failed: ", Ipv4Addr, " ", out.String())
		return errors.New("failed to reach host")
	}
	return nil
}

/*
*
Function to perform an arp scan on the network. This will allow us to see which IP's are available.
*/
func ArpScan(addrWSubnet string) string {
	output, err := utils.Execute("arp-scan", addrWSubnet)
	if err != nil {
		return err.Error()
	}
	return output
}

// IPv6NDP is gone (defect #66). It was the intended IPv6 announcer and had never
// been called by anything: it took no target address and omitted ndptool's `send`
// subcommand, so it could not have announced anything if it had been. The working
// form lives in announceCommand, on the path every caller already uses.

/*
*
Return network interface names
*/
func GetInterfaceNames() []string {
	log.Debug("Network Package - GetInerfacesNames()")
	links, err := netlink.LinkList()
	if err != nil {
		log.Debug("Network Package - GetInterfaceNames() Error retrieving network links via netlink. ", err)
		return nil
	}
	var interfaceNames []string
	for _, link := range links {
		attrs := link.Attrs()
		if attrs != nil && attrs.Slave == nil {
			interfaceNames = append(interfaceNames, attrs.Name)
		}
	}
	return interfaceNames
}

/*
*
Check if an interface exists on the local node
*/
func InterfaceExist(name string) (bool, netlink.Link) {
	log.Debug("Network Package - InterfaceExists()")
	link, err := netlink.LinkByName(name)
	if err != nil {
		log.Debug(err)
		return false, nil
	}
	return true, link
}

/*
*
Checks to see if an IP exists on an interface already
*/
func CheckIfIPExists(ipAddr string) (bool, string, error) {
	inventory, err := BuildIPInventory()
	if err != nil {
		return false, "", err
	}

	return inventory.Exists(ipAddr)
}
