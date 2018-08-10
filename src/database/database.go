/*
   PulseHA - HA Cluster Daemon
   Copyright (C) 2017-2018  Andrew Zak <andrew@pulseha.com>

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
package database

import (
	"github.com/Syleron/PulseHA/src/config"
	"github.com/Syleron/PulseHA/src/plugins"
	"sync"
)

type Database struct {
	sync.Mutex
	*config.Config
	Plugins plugins.Plugins
}

// Load, reads the config file and saves it to the db
// then validates the config
func (d *Database) Load() {
	d.Lock()
	defer d.Unlock()
	d.Config.Load()
	d.Config.Validate()
}

/**
Returns a copy of the config
*/
func (d *Database) GetConfig() *config.Config {
	return d.Config
}

/**
Should this save auto?
*/
func (d *Database) SetConfig(config *config.Config) {
	d.Lock()
	d.Config = config
	//set who we are might need to go somewhere else
	d.Unlock()
}
