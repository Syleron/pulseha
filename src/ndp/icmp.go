package ndp

import (
	"fmt"
	"encoding/binary"
	"golang.org/x/net/icmp"
	"net"
	"golang.org/x/net/ipv6"
)

/**

 */
type ICMP interface {
	String() string
	Marshal() ([]byte, error)
	Type() ipv6.ICMPType
}

/**

 */
func Checksum(body *[]byte, srcIP, dstIP net.IP) error {
	// from golang.org/x/net/icmp/message.go
	checksum := func(b []byte) uint16 {
		csumcv := len(b) - 1 // checksum coverage
		s := uint32(0)
		for i := 0; i < csumcv; i += 2 {
			s += uint32(b[i+1])<<8 | uint32(b[i])
		}
		if csumcv&1 == 0 {
			s += uint32(b[csumcv])
		}
		s = s>>16 + s&0xffff
		s = s + s>>16
		return ^uint16(s)
	}

	b := *body

	// remember origin length
	l := len(b)
	// generate pseudo header
	psh := icmp.IPv6PseudoHeader(srcIP, dstIP)
	// concat psh with b
	b = append(psh, b...)
	// set length of total packet
	off := 2 * net.IPv6len
	binary.BigEndian.PutUint32(b[off:off+4], uint32(l))
	// calculate checksum
	s := checksum(b)
	// set checksum in bytes and return original Body
	b[len(psh)+2] ^= byte(s)
	b[len(psh)+3] ^= byte(s >> 8)

	*body = b[len(psh):]
	return nil
}

/**

 */
func ParseMessage(b []byte) (ICMP, error) {
	if len(b) < 4 {
		//return nil, errMessageTooShort
		return nil, nil
	}

	icmpType := ipv6.ICMPType(b[0])
	var message ICMP

	switch icmpType {
	case ipv6.ICMPTypeRouterSolicitation:
		message = &ICMPRouterSolicitation{}

		if len(b) > 8 {
			options, err := parseOptions(b[8:])
			if err != nil {
				return nil, err
			}

			message.(*ICMPRouterSolicitation).Options = options
		}

		return message, nil

	case ipv6.ICMPTypeRouterAdvertisement:
		message = &ICMPRouterAdvertisement{
			HopLimit:       uint8(b[4]),
			ManagedAddress: false,
			OtherStateful:  false,
			HomeAgent:      false,
			RouterLifeTime: binary.BigEndian.Uint16(b[6:8]),
			ReachableTime:  binary.BigEndian.Uint32(b[8:12]),
			RetransTimer:   binary.BigEndian.Uint32(b[12:16]),
		}

		// parse flags
		if b[5]&0x80 > 0 {
			message.(*ICMPRouterAdvertisement).ManagedAddress = true
		}
		if b[5]&0x40 > 0 {
			message.(*ICMPRouterAdvertisement).OtherStateful = true
		}
		if b[5]&0x20 > 0 {
			message.(*ICMPRouterAdvertisement).HomeAgent = true
		}
		if b[5]&0x10 > 0 && b[5]&0x8 > 0 {
			message.(*ICMPRouterAdvertisement).RouterPreference = RouterPreferenceLow
		} else if b[5]&0x08 > 0 {
			message.(*ICMPRouterAdvertisement).RouterPreference = RouterPreferenceHigh
		}

		if len(b) > 16 {
			options, err := parseOptions(b[16:])
			if err != nil {
				return nil, err
			}

			message.(*ICMPRouterAdvertisement).Options = options
		}

		return message, nil

	case ipv6.ICMPTypeNeighborSolicitation:
		message = &ICMPNeighborSolicitation{
			TargetAddress: b[8:24],
		}

		if len(b) > 24 {
			options, err := parseOptions(b[24:])
			if err != nil {
				return nil, err
			}

			message.(*ICMPNeighborSolicitation).Options = options
		}

		return message, nil

	case ipv6.ICMPTypeNeighborAdvertisement:
		message = &ICMPNeighborAdvertisement{
			TargetAddress: b[8:24],
		}

		// parse flags
		if b[4]&0x80 > 0 {
			message.(*ICMPNeighborAdvertisement).Router = true
		}
		if b[4]&0x40 > 0 {
			message.(*ICMPNeighborAdvertisement).Solicited = true
		}
		if b[4]&0x20 > 0 {
			message.(*ICMPNeighborAdvertisement).Override = true
		}

		if len(b) > 24 {
			options, err := parseOptions(b[24:])
			if err != nil {
				return nil, err
			}

			message.(*ICMPNeighborAdvertisement).Options = options
		}

		return message, nil

	default:
		return nil, fmt.Errorf("message with type %d not supported", icmpType)
	}
}
