package network

import (
	"errors"
	"net"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

type partialInventory struct{ fail bool }

func (p partialInventory) LinkList() ([]netlink.Link, error) {
	return []netlink.Link{&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "first"}}, &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "second"}}}, nil
}
func (p partialInventory) AddrList(l netlink.Link, family int) ([]netlink.Addr, error) {
	if p.fail && l.Attrs().Name == "second" {
		return nil, errors.New("interface dump interrupted")
	}
	if family == unix.AF_INET {
		return []netlink.Addr{{IPNet: &net.IPNet{IP: net.ParseIP("192.0.2.10"), Mask: net.CIDRMask(32, 32)}}}, nil
	}
	return nil, nil
}
func TestInventoryNeverReturnsAPartialSnapshot(t *testing.T) {
	inv, err := readIPInventory(partialInventory{fail: true})
	if err == nil || inv != nil {
		t.Fatal("incomplete inventory could falsely confirm absence")
	}
	inv, err = readIPInventory(partialInventory{})
	if err != nil {
		t.Fatal(err)
	}
	held, _, err := inv.Exists("192.0.2.10")
	if err != nil || !held {
		t.Fatal("complete inventory lost held address")
	}
}
