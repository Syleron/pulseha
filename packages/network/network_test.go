package network

import (
	"testing"
)

func TestCheckIfIPExists(t *testing.T) {
	exists, iface, err := CheckIfIPExists("127.0.0.1")
	if err != nil {
		t.Error(err)
	}
	if !exists && iface != "lo" {
		t.Error("unable to find localhost on lo interface")
	}
}

func TestICMPv4(t *testing.T) {
	if err := ICMPv4("127.0.0.1/24"); err != nil {
		t.Error(err)
	}
}