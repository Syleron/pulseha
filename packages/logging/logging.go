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

package logging

import (
	log "github.com/sirupsen/logrus"
	"github.com/ssgreg/journalhook"
	"github.com/syleron/pulseha/packages/client"
	"github.com/syleron/pulseha/rpc"
	"os"
)

type Logging struct {
	Logger *log.Logger
	Level    rpc.LogsRequest_Level
	Hostname string
	Broadcast
}

type Broadcast func(funcName client.ProtoFunction, data interface{})

// NewLogger returns a new distributes logging object
func NewLogger(broadcast Broadcast) (Logging, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return Logging{}, err
	}
	// Set our memberlist
	logging := Logging{
		log.New(),
		rpc.LogsRequest_INFO,
		hostname,
		broadcast,
	}
	// Setup journal
	hook, err := journalhook.NewJournalHook()
	if err != nil {
		return Logging{}, err
	}
	// Attach our journal hook
	logging.Logger.Hooks.Add(hook)
	// Return with our logger
	return logging, nil
}

// Debug Send a debug message to the cluster
func (l *Logging) Debug(message string) {
	if l.Level == rpc.LogsRequest_DEBUG {
		l.Logger.Debugf("[%s] %s", l.Hostname, message)
		l.send(message, rpc.LogsRequest_DEBUG)
	}
}

// Warn sends a warning message to the cluster
func (l *Logging) Warn(message string) {
	l.Logger.Warnf("[%s] %s", l.Hostname, message)
	l.send(message, rpc.LogsRequest_WARNING)
}

// Info sends a info message to the cluster
func (l *Logging) Info(message string) {
	l.Logger.Infof("[%s] %s", l.Hostname, message)
	l.send(message, rpc.LogsRequest_INFO)
}

// Info sends a error message to the cluster
func (l *Logging) Error(message string) {
	l.Logger.Errorf("[%s] %s", l.Hostname, message)
	l.send(message, rpc.LogsRequest_ERROR)
}

// send broadcast a log message to the cluster
func (l *Logging) send(message string, level rpc.LogsRequest_Level) {
	l.Broadcast(client.SendLogs, &rpc.LogsRequest{
		Message: message,
		Node:    l.Hostname,
		Level:   level,
	})
}
