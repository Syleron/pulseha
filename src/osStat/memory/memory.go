/*
   PulseHA - HA Cluster Daemon
   Copyright (C) 2017-2019  Andrew Zak <andrew@linux.com>

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
package memory

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

type Stats struct {
	Total               uint64
	Used                uint64
	Buffers             uint64
	Cached              uint64
	Free                uint64
	Available           uint64
	Active              uint64
	Inactive            uint64
	SwapTotal           uint64
	SwapUsed            uint64
	SwapCached          uint64
	SwapFree            uint64
	MemAvailableEnabled bool
}

func Get() (*Stats, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return collectMemoryStats(file)
}

func collectMemoryStats(out io.Reader) (*Stats, error) {
	scanner := bufio.NewScanner(out)
	var memory Stats
	memStats := map[string]*uint64{
		"MemTotal":     &memory.Total,
		"MemFree":      &memory.Free,
		"MemAvailable": &memory.Available,
		"Buffers":      &memory.Buffers,
		"Cached":       &memory.Cached,
		"Active":       &memory.Active,
		"Inactive":     &memory.Inactive,
		"SwapCached":   &memory.SwapCached,
		"SwapTotal":    &memory.SwapTotal,
		"SwapFree":     &memory.SwapFree,
	}
	for scanner.Scan() {
		line := scanner.Text()
		i := strings.IndexRune(line, ':')
		if i < 0 {
			continue
		}
		fld := line[:i]
		if ptr := memStats[fld]; ptr != nil {
			val := strings.TrimSpace(strings.TrimRight(line[i+1:], "kB"))
			if v, err := strconv.ParseUint(val, 10, 64); err == nil {
				*ptr = v * 1024
			}
			if fld == "MemAvailable" {
				memory.MemAvailableEnabled = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan error for /proc/meminfo: %s", err)
	}
	memory.SwapUsed = memory.SwapTotal - memory.SwapFree
	if memory.MemAvailableEnabled {
		memory.Used = memory.Total - memory.Available
	} else {
		memory.Used = memory.Total - memory.Free - memory.Buffers - memory.Cached
	}
	return &memory, nil
}
