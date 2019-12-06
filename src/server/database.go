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
package server

import (
	"github.com/syleron/pulseha/src/config"
	"github.com/syleron/pulseha/src/logging"
)

type Database struct {
	Config             *config.Config
	Plugins            *Plugins
	MemberList         *MemberList
	Logging            logging.Logging
	StartDelay         bool
	StartInterval      int
}

func (d *Database) SetConfig(config *config.Config) {
	d.Config.Lock()
	defer d.Config.Unlock()
	d.Config = config
}
