package ndp

import (
	"strings"
	"net"
	"fmt"
	"golang.org/x/net/ipv6"
)

/**

 */
type ICMPNeighborAdvertisement struct {
	optionContainer
	Router        bool
	Solicited     bool
	Override      bool
	TargetAddress net.IP
}

/**

 */
func (p ICMPNeighborAdvertisement) String() string {
	m, _ := p.Marshal()
	s := fmt.Sprintf("%s, length %d, ", p.Type(), uint8(len(m)))
	s += fmt.Sprintf("tgt is %s, ", p.TargetAddress)
	s += "Flags ["
	if p.Router {
		s += "router "
	}
	if p.Solicited {
		s += "solicited "
	}
	if p.Override {
		s += "override"
	}
	s += "]\n"
	for _, o := range p.Options {
		s += fmt.Sprintf("    %s\n", o)
	}

	return strings.TrimSuffix(s, "\n")
}

/**
Type returns ipv6.ICMPTypeNeighborAdvertisement
 */
func (p ICMPNeighborAdvertisement) Type() ipv6.ICMPType {
	return ipv6.ICMPTypeNeighborAdvertisement
}

/**
Marshal returns byte slice representing this ICMPNeighborAdvertisement
 */
func (p ICMPNeighborAdvertisement) Marshal() ([]byte, error) {
	b := make([]byte, 8)
	// message header
	b[0] = uint8(p.Type())
	// b[1] = code, always 0
	// b[2:3] = checksum, calculated separately
	if p.Router {
		b[4] ^= 0x80
	}
	if p.Solicited {
		b[4] ^= 0x40
	}
	if p.Override {
		b[4] ^= 0x20
	}
	b = append(b, p.TargetAddress...)
	// add options
	om, err := p.Options.Marshal()
	if err != nil {
		return nil, err
	}

	b = append(b, om...)

	return b, nil
}