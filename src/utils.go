/*
    PulseHA - HA Cluster Daemon
    Copyright (C) 2017  Andrew Zak <andrew@pulseha.com>

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
package main


/**
 * Private - Check to see if we are in a configured cluster or not.
 */
func clusterCheck(c *Config) (bool) {
	c.localNode = "dave"
	if len(c.Nodes) > 0 {
		return true
	}
	return false
}

/**
 * Return the total number of configured nodes we have in our config.
 */
func clusterTotal(c *Config) (int) {
 return len(c.Nodes)
}


