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
	"github.com/coreos/go-log/log"
	"os"
	"path"
	"path/filepath"
	"plugin"
	"strconv"
	"github.com/Syleron/PulseHA/src/utils"
)

/**
 * Health Check plugin type
 */
type PluginHC interface {
	Name() string
	Version() float64
	Send() (bool, bool)
}

func LoadPlugins() ([]PluginHC, error) {
	var modules []PluginHC

	// Get project directory location
	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		log.Emergency(err)
	}

	utils.CreateFolder(dir + "/plugins")

	evtGlob := path.Join(dir+"/plugins", "/*.so")
	evt, err := filepath.Glob(evtGlob)

	if err != nil {
		return modules, err
	}

	var plugins []*plugin.Plugin

	for _, pFile := range evt {
		if plug, err := plugin.Open(pFile); err == nil {
			plugins = append(plugins, plug)
		}
	}

	for _, p := range plugins {
		symEvt, err := p.Lookup("PluginHC")

		if err != nil {
			log.Errorf("Plugin has no pluginType symbol: %v", err)
			continue
		}

		e, ok := symEvt.(PluginHC)

		if !ok {
			log.Error("Plugin is not of an PluginHC interface type")
			continue
		}

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
