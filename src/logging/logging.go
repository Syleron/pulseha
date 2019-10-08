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
package logging

import (
	log "github.com/Sirupsen/logrus"
	"github.com/syleron/pulseha/proto"
	"github.com/syleron/pulseha/src/client"
	"os"
)

type Logging struct {
	Level    proto.PulseLogs_Level
	Hostname string
	Broadcast
}

type Broadcast func(funcName client.ProtoFunction, data interface{})

// NewLogger Returns a new distributes logging object
func NewLogger(broadcast Broadcast) (Logging, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return Logging{}, err
	}
	// Set our memberlist
	logging := Logging{
		proto.PulseLogs_INFO,
		hostname,
		broadcast,
	}
	// Return with our logger
	return logging, nil
}

// Debug Send a debug message to the cluster
func (l *Logging) Debug(message string) {
	if l.Level == proto.PulseLogs_DEBUG {
		log.Debugf("[%s] %s", l.Hostname, message)
		l.send(message, proto.PulseLogs_DEBUG)
	}
}

// Warning Send a warning message to the cluster
func (l *Logging) Warn(message string) {
	log.Warnf("[%s] %s", l.Hostname, message)
	l.send(message, proto.PulseLogs_WARNING)
}

// Info Send an info message to the cluster
func (l *Logging) Info(message string) {
	log.Infof("[%s] %s", l.Hostname, message)
	l.send(message, proto.PulseLogs_INFO)
}

// Info Send an error message to the cluster
func (l *Logging) Error(message string) {
	log.Errorf("[%s] %s", l.Hostname, message)
	l.send(message, proto.PulseLogs_ERROR)
}

// Send a message to the cluster
func (l *Logging) send(message string, level proto.PulseLogs_Level) {
	l.Broadcast(client.SendLogs, &proto.PulseLogs{
		Message: message,
		Node:    l.Hostname,
		Level:   level,
	})
}
