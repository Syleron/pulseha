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
package uptime

import "testing"

func TestGetUptime(t *testing.T) {
	uptime, err := Get()
	if err != nil {
		t.Fatalf("error should be nil but got: %v", err)
	}
	if uptime.Seconds() <= 0 {
		t.Errorf("invalid uptime value: %v", uptime)
	}
	t.Logf("uptime value: %+v", uptime)
}