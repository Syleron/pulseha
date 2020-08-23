/*
   PulseHA - HA Cluster Daemon
   Copyright (C) 2017-2020  Andrew Zak <andrew@linux.com>

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
package network

import (
	"reflect"
	"strings"
	"testing"
)

func TestGetNetwork(t *testing.T) {
	networks, err := Get()
	if err != nil {
		t.Fatalf("error should be nil but got: %v", err)
	}
	for _, network := range networks {
		if network.Name == "en0" && network.RxBytes <= 0 {
			t.Errorf("invalid network value: %+v", network)
		}
	}
	t.Logf("networks value: %+v", networks)
}

func TestCollectNetworkStats(t *testing.T) {
	got, err := collectNetworkStats(strings.NewReader(
		`Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
 wlan0: 1188035151  850857    0    0    0     0          0         0 49774221  428282    0    0    0     0       0          0
    lo: 1292817    9913    0    0    0     0          0         0  1292817    9913    0    0    0     0       0          0
  eth0: 26054426   73542    0    0    0     0          0         0 12352148   58473    0    0    0     0       0          0
  eth1:183651236    3482    0    0    0     0          0         0 93127469    1924    0    0    0     0       0          0
`))
	if err != nil {
		t.Fatalf("error should be nil but got: %v", err)
	}
	expected := []Stats{
		{"wlan0", 1188035151, 49774221},
		{"eth0", 26054426, 12352148},
		{"eth1", 183651236, 93127469},
	}
	if !reflect.DeepEqual(got, expected) {
		t.Errorf("invalid network value: %+v (expected: %+v)", got, expected)
	}
}
