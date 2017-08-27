package modules

import (
	log "github.com/Sirupsen/logrus"
	"path"
	"path/filepath"
	"plugin"
	"github.com/Syleron/Pulse/src/utils"
)

type Plugin interface {
	PluginName() string
}

type pluginType interface {
	Name() string
	Decode([]byte) (Plugin, error)
}

func LoadPlugins() ([]pluginType, error) {
	var modules []pluginType

	utils.CreateFolder("./modules")

	evtGlob := path.Join("./modules", "/*.so")
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
		symEvt, err := p.Lookup("EventType")

		if err != nil {
			log.Errorf("Event Type has no eventType symbol: %v", err)
			continue
		}

		e, ok := symEvt.(pluginType)

		if !ok {
			log.Errorf("Event Type is not an Event interface type")
			continue
		}

		modules = append(modules, e)
	}

	log.Infof("%v plugins loaded", len(modules))

	return modules, nil
}