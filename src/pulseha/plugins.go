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
	log "github.com/sirupsen/logrus"
	"path"
	"path/filepath"
	"plugin"
	"strconv"
)

// PluginHC is the health check object structure
type PluginHC interface {
	Name() string
	Version() float64
	Weight() int64
	Send() error
}

// PluginNet is the network plugin object structure
type PluginNet interface {
	Name() string
	Version() float64
	BringUpIPs(iface string, ips []string) error
	BringDownIPs(iface string, ips []string) error
}

// PluginGen is the general plugin object structure
type PluginGen interface {
	Name() string
	Version() float64
	Run(db *Database) error
	OnMemberListStatusChange(members []Member)
}

// Plugins object structure which stores our plugins
type Plugins struct {
	modules []*Plugin
}

// Plugin Plugin object structure
type Plugin struct {
	Name    string
	Version float64
	Type    interface{}
	Plugin  interface{}
}

type pluginType int

const (
	PluginHealthCheck pluginType = 1 + iota
	PluginNetworking
	PluginGeneral
)

var pluginTypeNames = []string{
	"PluginHC",
	"PluginNet",
	"PluginGeneral",
}

func (p pluginType) String() string {
	return pluginTypeNames[p-1]
}

// Setup defines each type of plugin to load
func (p *Plugins) Setup() {
	// Join any number of file paths into a single path
	evtGlob := path.Join("/usr/local/lib/pulseha/", "/*.so")
	// Return all the files that match the file name pattern
	evt, err := filepath.Glob(evtGlob)
	// handle errors
	if err != nil {
		panic(err.Error())
	}
	// list of plugins
	var plugins []*plugin.Plugin
	// Load them
	for _, pFile := range evt {
		if plug, err := plugin.Open(pFile); err == nil {
			plugins = append(plugins, plug)
		} else {
			log.Warning("Unable to load plugin " + pFile + ". Perhaps it is out of date?")
			log.Debug(pFile + " - " + err.Error())
		}
	}
	p.Load(PluginHealthCheck, plugins)
	p.Load(PluginNetworking, plugins)
	p.Load(PluginGeneral, plugins)
	p.Validate()
	if len(p.modules) > 0 {
		var pluginNames string = ""
		for _, plgn := range p.modules {
			pluginNames += plgn.Name + "(v" + strconv.FormatFloat(plgn.Version, 'f', -1, 32) + ") "
		}
		log.Infof("Plugins loaded (%v): %v", len(p.modules), pluginNames)
	}
}

// Validate ensures that minimal required plugins are loaded.
func (p *Plugins) Validate() {
	// make sure we have a networking plugin
	if p.GetNetworkingPlugin() == nil {
		log.Warning("No networking plugin loaded. PulseHA now in monitoring mode..")
	}
}

// Load is used to load a particular plugin type.
// TODO: This needs to be cleaned up so code can be reused instead of repeated so much
func (p *Plugins) Load(pluginType pluginType, pluginList []*plugin.Plugin) {
	// TODO: Note: Unfortunately a switch statement must be used as you cannot dynamically typecast a variable.
	for _, plugin := range pluginList {
		switch pluginType {
		case PluginGeneral:
			symEvt, err := plugin.Lookup(pluginType.String())
			if err != nil {
				log.Debugf("Plugin does not match pluginType symbol: %v", err)
				continue
			}
			e, ok := symEvt.(PluginGen)
			if !ok {
				continue
			}
			// Create a new instance of plugins
			newPlugin := &Plugin{
				Name:    e.Name(),
				Version: e.Version(),
				Type:    pluginType,
				Plugin:  e,
			}
			// Add to the list of plugins
			p.modules = append(p.modules, newPlugin)
			go e.Run(DB)
		case PluginHealthCheck:
			symEvt, err := plugin.Lookup(pluginType.String())
			if err != nil {
				log.Debugf("Plugin does not match pluginType symbol: %v", err)
				continue
			}
			e, ok := symEvt.(PluginHC)
			if !ok {
				continue
			}
			// Create a new instance of plugins
			newPlugin := &Plugin{
				Name:    e.Name(),
				Version: e.Version(),
				Type:    pluginType,
				Plugin:  e,
			}
			// Add to the list of plugins
			p.modules = append(p.modules, newPlugin)
		case PluginNetworking:
			// Make sure we are not loading another networking plugin.
			// Only one networking plugin can be loaded at one time.
			if p.GetNetworkingPlugin() != nil {
				continue
			}
			symEvt, err := plugin.Lookup(pluginType.String())
			if err != nil {
				log.Debugf("Plugin does not match pluginType symbol: %v", err)
				continue
			}
			e, ok := symEvt.(PluginNet)
			if !ok {
				continue
			}
			// Create a new instance of plugins
			newPlugin := &Plugin{
				Name:    e.Name(),
				Version: e.Version(),
				Type:    pluginType,
				Plugin:  e,
			}
			// Add to the list of plugins
			p.modules = append(p.modules, newPlugin)
		}
	}
}

// GetHealthCheckPlugins is used to gather a slice of health check plugins.
func (p *Plugins) GetHealthCheckPlugins() []*Plugin {
	modules := []*Plugin{}
	for _, plgin := range p.modules {
		if plgin.Type == PluginHealthCheck {
			modules = append(modules, plgin)
		}
	}
	return modules
}

// GetNetworkingPlugin is used to gather a slice of networking plugins
func (p *Plugins) GetNetworkingPlugin() *Plugin {
	for _, plgin := range p.modules {
		if plgin.Type == PluginNetworking {
			return plgin
		}
	}
	return nil
}

// GetGeneralPlugin is used to gather a slice of general plugins
func (p *Plugins) GetGeneralPlugin() []*Plugin {
	modules := []*Plugin{}
	for _, plgin := range p.modules {
		if plgin.Type == PluginGeneral {
			modules = append(modules, plgin)
		}
	}
	return modules
}
