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
package cpu

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode"
)

type Stats struct {
	User                uint64
	Nice                uint64
	System              uint64
	Idle                uint64
	Iowait              uint64
	Irq                 uint64
	Softirq             uint64
	Steal               uint64
	Guest               uint64
	GuestNice           uint64
	Total               uint64
	CPUCount, StatCount int
}

type cpuStat struct {
	name string
	ptr  *uint64
}

func Get() (*Stats, error) {
	file, err := os.Open("/proc/stat")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return collectCPUStats(file)
}

func collectCPUStats(out io.Reader) (*Stats, error) {
	scanner := bufio.NewScanner(out)
	var cpu Stats
	cpuStats := []cpuStat{
		{"user", &cpu.User},
		{"nice", &cpu.Nice},
		{"system", &cpu.System},
		{"idle", &cpu.Idle},
		{"iowait", &cpu.Iowait},
		{"irq", &cpu.Irq},
		{"softirq", &cpu.Softirq},
		{"steal", &cpu.Steal},
		{"guest", &cpu.Guest},
		{"guest_nice", &cpu.GuestNice},
	}
	if !scanner.Scan() {
		return nil, fmt.Errorf("failed to scan /proc/stat")
	}
	valStrs := strings.Fields(scanner.Text())[1:]
	cpu.StatCount = len(valStrs)
	for i, valStr := range valStrs {
		val, err := strconv.ParseUint(valStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to scan %s from /proc/stat", cpuStats[i].name)
		}
		*cpuStats[i].ptr = val
		cpu.Total += val
	}
	cpu.Total -= cpu.Guest
	cpu.Total -= cpu.GuestNice
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "cpu") && unicode.IsDigit(rune(line[3])) {
			cpu.CPUCount++
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan error for /proc/stat: %s", err)
	}
	return &cpu, nil
}
