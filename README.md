```
   ___       __        __ _____ 
  / _ \__ __/ /__ ___ / // / _ |
 / ___/ // / (_-</ -_) _  / __ |
/_/   \_,_/_/___/\__/_//_/_/ |_|

```

<a href="https://travis-ci.org/Syleron/PulseHA"><img src="https://travis-ci.org/Syleron/PulseHA.svg?branch=master"><a/>
<a href="https://godoc.org/github.com/Syleron/PulseHA"><img src="https://godoc.org/github.com/Syleron/PulseHA?status.svg"><a/>
<a href="https://www.gnu.org/licenses/agpl-3.0"><img src="https://img.shields.io/badge/License-AGPL%20v3-blue.svg"><a/>
  
## Overview
PulseHA is an active-passive cluster communications manager (CCM) daemon written in GO that provides a means of communication and membership monitoring within a network cluster. By utilising Remote Procedure Calls (RPC) using Google's GRPC, PulseHA provides a reliable method of communication to ensure network high availability.

## Why
PulseHA attempts to solve high availability with a more simple approach but without restricting functionality with the use of additional custom plugins.

## Features
- Remote procedural calls via GRPC
- Active/Passive cluster membership monitoring
- Failure detection and recovery
- Floating IP fencing (requires network plugin)
- IPv4 & IPv6 support
- Plugin support (for additional health checks and networking logic)
- Command line interface (CLI)

## Prerequisites

* Go v1.9 or later
* Protoc v3.4 or later

## System Requirements

* Centos 7+

## Build & Install

First you will need to clone this repository into `$GOPATH/src/github.com/Syleron/PulseHA` and execute the following command(s):


```
$ sudo make
...
```

Lastly, you can install PulseHA by executing the following:

```
$ sudo make install
...
```

## Commands

### Cluster

Cluster status

```
$ pulsectl status
```

Create a cluster

```
$ pulsectl create <bind ip> <bind port>
```

Join a cluster

```
$ pulsectl join -bind-ip=<bind ip> -bind-port=<bind port> -token=<cluster token> <dest ip> <dest port>
```

Leave a cluster

```
$ pulsectl leave
```

Remove from a cluster

```
$ pulsectl remove <member hostname>
```

Promote cluster member to become active

```
$ pulsectl promote <member hostname>
```

Generate new cluster token

```
$ pulsectl token
```

### Groups

Create Floating IP Group

```
$ pulsectl -name=<group name> new
```

Delete Floating IP Group

```
$ pulsectl -name=<group name> delete
```

Assign Floating IP Group

```
$ pulsectl -name=<group name> -node=<member hostname> -iface=<net iface> assign
```

Un-assign Floating IP Group

```
$ pulsectl -name=<group name> -node=<member hostname> -iface=<net iface> unassign
```

Add Floating IP to a Floating IP Group

```
$ pulsectl groups -name=<group name> -node=<member hostname> -ips=<ip CIDR> -iface=<net iface> add
```

Remove Floating IP to a Floating IP Group

```
$ pulsectl groups -name=<group name> -node=<member hostname> -ips=<ip CIDR> -iface=<net iface> remove
```

### Certificates

Re-generate TLS certificates

```
$ pulsectl cert <bind ip>
```

### Config

Update/Change config value

```
$ pulsectl config <config key> <config value>
```

### Network

Re-sync network interfaces

```
$ pulsectl network resync
```

### Help

Pulsectl help

```
$ pulsectl help
```

Pulsectl version

```
$ pulsectl version
```

## Plugins

### PulseHA-Netcore



### PulseHA-Email-Alerts

### PulseHA-Ping-Groups

### PulseHA-Serial

## Acknowledgments

Thank you to all authors who have and continue to contribute to this project.

- [Ben Cabot](https://github.com/bencabot) for your contributions.

## License
PulseHA source code is available under the AGPL License which can be found in the LICENSE file.

Copyright (c) 2017-2022 Andrew Zak <andrew@linux.com>
