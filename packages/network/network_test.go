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
