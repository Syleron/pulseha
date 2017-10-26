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

import (
	"github.com/Syleron/PulseHA/src/utils"
	log "github.com/Sirupsen/logrus"
	"os"
	"path"
	"path/filepath"
	"plugin"
	"strconv"
)

/**
Health Check plugin type
 */
type PluginHC interface {
	Name() string
	Version() float64
	Send() (bool, bool)
}

/**
Networking plugin type
 */
type PluginNet interface {
	Name() string
	Version() float64
	BringUpIPs() error
	BringDownIPs() error
}

/**
Plugins struct
 */
type Plugins struct {
	modules []Plugin
}

/**
Struct for a specific plugin
 */
type Plugin struct {
	Name string
	Type interface{}
}

/**
TODO: Note: Make sure that the modules slice is empty before adding.
 */
func (p *Plugins) Load() error {
	// Get project directory location
	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		log.Fatal(err)
	}
	// Create plugin folder
	utils.CreateFolder(dir + "/plugins")
	evtGlob := path.Join(dir+"/plugins", "/*.so")
	evt, err := filepath.Glob(evtGlob)

	if err != nil {
		panic(err.Error())
	}

	var plugins []*plugin.Plugin

	for _, pFile := range evt {
		if plug, err := plugin.Open(pFile); err == nil {
			plugins = append(plugins, plug)
		}
	}
	// Load all of the plugins
	for _, p := range plugins {
		symEvt, err := p.Lookup("PluginHC")
		// make sure we have loaded a plugin type
		if err != nil {
			log.Debugf("Plugin has no pluginType symbol: %v", err)
			continue
		}
		// check if the loaded plugin is of type PluginHC
		e, ok := symEvt.(PluginHC)
		// the plugin we are attempting to load is not a valid health check plugin
		if !ok {
			continue
		}
		// add the plugin to the slice
		modules = append(modules, e)
	}

	if len(modules) > 0 {
		var pluginNames string = ""
		for _, plgn := range modules {
			pluginNames += plgn.Name() + "(v" + strconv.FormatFloat(plgn.Version(), 'f', -1, 32) + ") "
		}
		log.Infof("Plugins loaded (%v): %v", len(modules), pluginNames)
	}

	return modules, nil
}

/**
Perform any plugins validation here
 */
func (p *Plugins) validate() {
}

/**
Returns a slice of health check plugins
 */
func (p *Plugins) getHealthCheckPlugins() []*Plugin {
	return nil
}

/**
Returns a single networking plugin (as you should only ever have one loaded)
 */
func (p *Plugins) getNetworkingPlugin() *Plugin {
	return &Plugin{}
}
