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
package disk

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

type Stats struct {
	Name            string
	ReadsCompleted  uint64
	WritesCompleted uint64
}

func Get() ([]Stats, error) {
	file, err := os.Open("/proc/diskstats")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return collectDiskStats(file)
}

func collectDiskStats(out io.Reader) ([]Stats, error) {
	scanner := bufio.NewScanner(out)
	var diskStats []Stats
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 14 {
			continue
		}
		name := fields[2]
		readsCompleted, err := strconv.ParseUint(fields[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse reads completed of %s", name)
		}
		writesCompleted, err := strconv.ParseUint(fields[7], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse writes completed of %s", name)
		}
		diskStats = append(diskStats, Stats{
			Name:            name,
			ReadsCompleted:  readsCompleted,
			WritesCompleted: writesCompleted,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan error for /proc/diskstats: %s", err)
	}
	return diskStats, nil
}
