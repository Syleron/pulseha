/*
   PulseHA - HA Cluster Daemon
   Copyright (C) 2017-2021  Andrew Zak <andrew@linux.com>

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published
   by the Free Software Foundation, either version 3 of the License, or
   (at your option) any later version.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/
package utils

import (
	"testing"
)

func TestValidIPAddress(t *testing.T) {
	err := ValidIPAddress("192.168.63.200/24")

	if err != nil {
		t.Errorf(err.Error())
	}
}

func TestScheduler(t *testing.T) {
	var counter int = 1

	Scheduler(func() bool {
		if counter < 3 {
			counter++
			return false
		} else {
			return true
		}
	}, 1)

	if counter != 3 {
		t.Fail()
	}
}

func TestGetHostname(t *testing.T) {
	_, err := GetHostname()

	if err != nil {
		t.Errorf(err.Error())
	}
}

func TestSplitIpPort(t *testing.T) {
	ip, port, err := SplitIpPort("127.0.0.1:8080")

	if ip != "127.0.0.1" || port != "8080" || err != nil {
		t.Fail()
	}
}

func TestIsIPv6(t *testing.T) {
	validIPv6 := IsIPv6("2001:db8:85a3::8a2e:370:7334")

	if !validIPv6 {
		t.Fail()
	}
}

func TestIsIPv4(t *testing.T) {
	valid := IsIPv4("127.0.0.1")

	if !valid {
		t.Fail()
	}
}

func TestSanitizeIPv6(t *testing.T) {
	ipv6 := SanitizeIPv6("[2001:db8:85a3::8a2e:370:7334]")

	if ipv6 != "2001:db8:85a3::8a2e:370:7334" {
		t.Fail()
	}
}

func TestFormatIPv6(t *testing.T) {
	ipv6 := FormatIPv6("2001:db8:85a3::8a2e:370:7334")

	if ipv6 != "[2001:db8:85a3::8a2e:370:7334]" {
		t.Fail()
	}
}

func TestIsCIDR(t *testing.T) {
	isCIDR := IsCIDR("127.0.0.1/24")

	if !isCIDR {
		t.Fail()
	}
}

func TestGetCIDR(t *testing.T) {
	ip, mask := GetCIDR("127.0.0.1/24")

	if ip == nil || mask == nil {
		t.Fail()
	}
}

func TestHasPort(t *testing.T) {
	portCheck0 := HasPort("127.0.0.1")
	portCheck1 := HasPort("127.0.0.1:8080")
	portCheck2 := HasPort("2001:db8:85a3::8a2e:370:7334")
	portCheck3 := HasPort("[2001:db8:85a3::8a2e:370:7334i8080]:8080")

	if portCheck0 || !portCheck1 || portCheck2 || !portCheck3 {
		t.Fail()
	}
}
