package discovery

import (
	"fmt"
	log "github.com/Sirupsen/logrus"
	"golang.org/x/net/ipv4"
	"net"
	"strconv"
	"sync"
)

type IPVersion uint

const (
	IPv4 IPVersion = 4
	IPv6 IPVersion = 6
	MulticastAddress = "239.255.255.250"
)

type Service struct {}

type Registry struct {
	Settings Settings
	received map[string][]byte
	sync.Mutex
}

type Settings struct {
	Port string
	Payload []byte
	multicastAddressNumbers net.IP
}

type Discovered struct {
	// Address is the local address of a discovered peer.
	Address string
	// Payload is the associated payload from discovered peer.
	Payload []byte
}
func (d Discovered) String() string {
	return fmt.Sprintf("address: %s, payload: %s", d.Address, d.Payload)
}

/**
Used to find other instances on the network
 */
func (r *Registry) Discover() {}

/**
Listen in for other instances on the network
 */
func (r *Registry) Listen() {
	// Setup
	r.Settings.multicastAddressNumbers = net.ParseIP(MulticastAddress)

	// Variables
	address := net.JoinHostPort(MulticastAddress, r.Settings.Port)
	portNum, err := strconv.Atoi(r.Settings.Port)

	if err != nil {
		return
	}

	c, err := net.ListenPacket(fmt.Sprintf("udp%d", IPv4), address)

	if err != nil {
		return
	}

	defer c.Close()

	ifaces, err := net.Interfaces()

	if err != nil {
		return
	}

	group := r.Settings.multicastAddressNumbers

	var p2 interface{}

	p2 = ipv4.NewPacketConn(c)

	for i := range ifaces {
		p2.(*ipv4.PacketConn).JoinGroup(&ifaces[i], &net.UDPAddr{IP: group, Port: portNum})
	}

	log.Info("PulseHA discovery initialised..")

	// Read from the socket
	for {
		buffer := make([]byte, 66507)
		var (
			n       int
			src     net.Addr
			errRead error
		)
		n, _, src, err = p2.(*ipv4.PacketConn).ReadFrom(buffer)
		if err != nil {
			err = errRead
			return
		}
		srcHost, _, _ := net.SplitHostPort(src.String())

		r.Lock()
		if _, ok := r.received[srcHost]; !ok {
			r.received[srcHost] = buffer[:n]
		}
		r.Unlock()

		log.Info(srcHost + " - " + string(buffer[:n]) + " discovered")
	}
}

func New(s Settings) *Registry {
	log.Debug("discovery:New() Created new discovery object")
	r := new(Registry)
	r.Settings = s
	return r
}
