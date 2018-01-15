package ndp

import (
	"fmt"
	"strings"
	"golang.org/x/net/ipv6"
)

/**

 */
type ICMPRouterSolicitation struct {
	optionContainer
}

/**

 */
func (p ICMPRouterSolicitation) String() string {
	m, _ := p.Marshal()
	s := fmt.Sprintf("%s, length %d\n", p.Type(), uint8(len(m)))
	for _, o := range p.Options {
		s += fmt.Sprintf("    %s\n", o)
	}

	return strings.TrimSuffix(s, "\n")
}

/**
Type returns ipv6.ICMPTypeRouterSolicitation
 */
func (p ICMPRouterSolicitation) Type() ipv6.ICMPType {
	return ipv6.ICMPTypeRouterSolicitation
}

/**
Marshal returns byte slice representing this ICMPRouterSolicitation
 */
func (p ICMPRouterSolicitation) Marshal() ([]byte, error) {
	b := make([]byte, 8)
	// message header
	b[0] = uint8(p.Type())
	// b[1] = code, always 0
	// b[2:3] = checksum, calculated separately
	// add options
	om, err := p.Options.Marshal()
	if err != nil {
		return nil, err
	}

	b = append(b, om...)
	return b, nil
}