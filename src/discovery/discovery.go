package discovery

import (
	"fmt"
	log "github.com/Sirupsen/logrus"
	"golang.org/x/net/ipv4"
	"net"
	"strconv"
	"sync"
	"time"
)

type IPVersion uint

const (
	IPv4 IPVersion = 4
	IPv6 IPVersion = 6
	MulticastAddress = "239.255.255.250"
	TimeLimit = 10
)

type DiscoverHandler interface {
	DiscoveredHost(string)
}

type Service struct {}

type Registry struct {
	Settings Settings
	received map[string][]byte
	stopChan chan bool
	handler DiscoverHandler
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
func (r *Registry) Discover() {
	log.Debug("PulseHA discovery search initialised..")
	address := net.JoinHostPort(MulticastAddress, r.Settings.Port)
	portNum, err := strconv.Atoi(r.Settings.Port)

	// get interfaces
	ifaces, err := net.Interfaces()

	if err != nil {
		return
	}

	// Open up a connection
	c, err := net.ListenPacket(fmt.Sprintf("udp%d", IPv4), address)
	if err != nil {
		return
	}
	defer c.Close()

	group := r.Settings.multicastAddressNumbers

	var p2 interface{}

	p2 = ipv4.NewPacketConn(c)

	for i := range ifaces {
		p2.(*ipv4.PacketConn).JoinGroup(&ifaces[i], &net.UDPAddr{IP: group, Port: portNum})
	}

	ticker := time.NewTicker(TimeLimit * time.Second)
	defer ticker.Stop()
	start := time.Now()

	for t := range ticker.C {
		exit := false

		select {
			case <-r.stopChan:
			exit = true
			default:
		}

		// write to multicast
		dst := &net.UDPAddr{IP: group, Port: portNum}
		for i := range ifaces {
			p24 := p2.(*ipv4.PacketConn)
			if errMulticast := p24.SetMulticastInterface(&ifaces[i]); errMulticast != nil {
				log.Print(errMulticast)
				continue
			}
			p24.SetMulticastTTL(2)
			if _, errMulticast := p24.WriteTo([]byte(r.Settings.Payload), nil, dst); errMulticast != nil {
				log.Print(errMulticast)
				continue
			}
		}

		if exit || TimeLimit * time.Second > 0 && t.Sub(start) > TimeLimit * time.Second {
			break
		}
	}
}

/**
Stop attempting to find new service on the network
 */
func (r *Registry) Reset() {
	r.stopChan <- true
}

/**
Listen in for other instances on the network
 */
func (r *Registry) Listen() {
	log.Info("PulseHA discovery listening..")
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

		// Inform our handler
		r.handler.DiscoveredHost(string(buffer[:n]))
	}
}

func New(s Settings, h DiscoverHandler) *Registry {
	log.Debug("Discovery:New() Created new discovery object")
	r := new(Registry)
	if len(s.Payload) == 0 {
		s.Payload = []byte("We are aware only of the empty space in the forest, which only yesterday was filled with trees.")
	}
	r.Settings = s
	r.stopChan = make(chan bool)
	r.handler = h
	return r
}
