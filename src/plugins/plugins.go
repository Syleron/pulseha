package plugins

import (
    log "github.com/Sirupsen/logrus"
    "github.com/Syleron/Pulse/src/utils"
    "plugin"
)

type Plugin interface {
	PluginName() string
}

type pluginType interface {
	Name() string
	Decode([]byte) (Plugin, error)
}

func LoadPlugins() ([]pluginType, error) {
    var plugins []pluginType
    
    log.Info("[Plugins] Loading..")
    
	return plugins, nil
}