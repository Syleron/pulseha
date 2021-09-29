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
package network

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

type Stats struct {
	Name    string
	RxBytes uint64
	TxBytes uint64
}

func Get() ([]Stats, error) {
	file, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return collectNetworkStats(file)
}

func collectNetworkStats(out io.Reader) ([]Stats, error) {
	scanner := bufio.NewScanner(out)
	var networks []Stats
	for scanner.Scan() {
		kv := strings.SplitN(scanner.Text(), ":", 2)
		if len(kv) != 2 {
			continue
		}
		fields := strings.Fields(kv[1])
		if len(fields) < 16 {
			continue
		}
		name := strings.TrimSpace(kv[0])
		if name == "lo" {
			continue
		}
		rxBytes, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse rxBytes of %s", name)
		}
		txBytes, err := strconv.ParseUint(fields[8], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse txBytes of %s", name)
		}
		networks = append(networks, Stats{Name: name, RxBytes: rxBytes, TxBytes: txBytes})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan error for /proc/net/dev: %s", err)
	}
	return networks, nil
}
