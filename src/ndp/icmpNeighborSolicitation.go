package ndp

import (
	"net"
	"fmt"
	"strings"
	"golang.org/x/net/ipv6"
)

type ICMPNeighborSolicitation struct {
	optionContainer
	TargetAddress net.IP
}

/**

 */
func (p ICMPNeighborSolicitation) String() string {
	m, _ := p.Marshal()
	s := fmt.Sprintf("%s, length %d, ", p.Type(), uint8(len(m)))
	s += fmt.Sprintf("who has %s\n", p.TargetAddress)
	for _, o := range p.Options {
		s += fmt.Sprintf("    %s\n", o)
	}

	return strings.TrimSuffix(s, "\n")
}

/**
Type returns ipv6.ICMPTypeNeighborSolicitation
 */
func (p ICMPNeighborSolicitation) Type() ipv6.ICMPType {
	return ipv6.ICMPTypeNeighborSolicitation
}

/**
Marshal returns byte slice representing this ICMPNeighborSolicitation
 */
func (p ICMPNeighborSolicitation) Marshal() ([]byte, error) {
	b := make([]byte, 8)
	// message header
	b[0] = uint8(p.Type())
	// b[1] = code, always 0
	// b[2:3] = checksum, calculated separately
	b = append(b, p.TargetAddress...)
	// add options
	om, err := p.Options.Marshal()
	if err != nil {
		return nil, err
	}

	b = append(b, om...)
	return b, nil
}