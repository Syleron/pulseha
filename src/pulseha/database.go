// PulseHA - HA Cluster Daemon
// Copyright (C) 2017-2021  Andrew Zak <andrew@linux.com>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package pulseha

import (
	"github.com/syleron/pulseha/packages/config"
	"github.com/syleron/pulseha/packages/logging"
)

// Database defines our database object
type Database struct {
	Config        *config.Config
	Plugins       *Plugins
	MemberList    *MemberList
	Logging       logging.Logging
	StartDelay    bool
	StartInterval int
}

// SetConfig replaces our current in memory config with another
func (d *Database) SetConfig(config *config.Config) {
	d.Config.Lock()
	defer d.Config.Unlock()
	d.Config = config
}
