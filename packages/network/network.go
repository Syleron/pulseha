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

func parseTargetIP(ipAddr string) (net.IP, error) {
	log.Debug("CheckIfIPExists called", "searchIP", ipAddr)

	if strings.Contains(ipAddr, "/") {
		parsedIP, _, err := net.ParseCIDR(ipAddr)
		if err != nil {
			log.Debug("CheckIfIPExists invalid CIDR", "input", ipAddr, "error", err)
			return nil, err
		}
		if normalized, ok := normalizeIP(parsedIP); ok {
			return normalized, nil
		}
		log.Debug("CheckIfIPExists unsupported IP family", "input", ipAddr)
		return nil, errors.New("unsupported IP address family for: " + ipAddr)
	}

	parsedIP := net.ParseIP(ipAddr)
	if parsedIP == nil {
		log.Debug("CheckIfIPExists invalid IP", "input", ipAddr)
		return nil, errors.New("invalid IP address: " + ipAddr)
	}
	if normalized, ok := normalizeIP(parsedIP); ok {
		return normalized, nil
	}
	log.Debug("CheckIfIPExists unsupported IP family", "input", ipAddr)
	return nil, errors.New("unsupported IP address family for: " + ipAddr)
}

/*
*
Send Gratuitous ARP to automagically tell the router who has the new floating IP
NOTE: This function assumes the OS is LINUX and has "arping" installed.
*/
func SendGARP(iface, ip string) error {
	exists, _ := InterfaceExist(iface)
	if !exists {
		log.Error("Unable to GARP as the network interface does not exist")
		return errors.New("network interface does not exist")
	}
	var garpIP net.IP
	if parsedIP := net.ParseIP(ip); parsedIP != nil {
		garpIP = parsedIP
	} else {
		parsedIP, _, err := net.ParseCIDR(ip)
		if err != nil {
			log.Error("failed to GARP. Cannot parse IP address", "value", ip, "error", err)
			return err
		}
		garpIP = parsedIP
	}
	log.Debug("Sending gratuitous arp for " + garpIP.String() + " on interface " + iface)
	_, err := utils.ExecuteWithTimeout(garpTimeout, "arping", "-U", "-c", "5", "-I", iface, garpIP.String())
	if err != nil {
		log.Error("failed to GARP. " + err.Error())
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
func SendGARPBatch(iface string, ips []string) error {
	if len(ips) == 0 {
		return nil
	}
	if exists, _ := InterfaceExist(iface); !exists {
		log.Error("Unable to GARP as the network interface does not exist", "iface", iface)
		return errors.New("network interface does not exist")
	}
	return sendGARPBatch(iface, ips, SendGARP)
}

// announceFunc announces a single address on an interface.
type announceFunc func(iface, ip string) error

// sendGARPBatch is the fan-out half of SendGARPBatch, with the announcement
// injected. The real one execs arping, so this is where the concurrency bound and
// the failure reporting can be tested without a network or a Linux host.
func sendGARPBatch(iface string, ips []string, announce announceFunc) error {
	sem := make(chan struct{}, garpFanout)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed []string

	for _, ip := range ips {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if err := announce(iface, ip); err != nil {
				mu.Lock()
				failed = append(failed, ip)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(failed) > 0 {
		return fmt.Errorf("failed to announce %d of %d address(es) on %s (e.g. %s)",
			len(failed), len(ips), iface, strings.Join(failed[:min(3, len(failed))], ", "))
	}
	return nil
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

/*
*
Send the eq. of IPv4 arping with IPv6
*/
func IPv6NDP(ipv6Iface string) string {
	output, err := utils.Execute("ndptool", "-t", "na", "-U", "-i", ipv6Iface)
	if err != nil {
		return err.Error()
	}
	return output
}

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
