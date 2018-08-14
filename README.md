<p align="center">
<img src="pulse-logo.png" width="250">
<br><br>
<a href="https://godoc.org/github.com/Syleron/PulseHA"><img src="https://godoc.org/github.com/Syleron/PulseHA?status.svg"><a/>
<a href="https://www.gnu.org/licenses/agpl-3.0"><img src="https://img.shields.io/badge/License-AGPL%20v3-blue.svg"><a/>
</p>
  
## Overview
PulseHA is an active-passive cluster communications manager (CCM) daemon written in GO that provides a means of communication and membership monitoring within a network cluster. By utilising Remote Procedure Calls (RPC) using Google's GRPC, PulseHA provides a reliable method of communication to ensure network high availability.

## Prerequisites

* Go v9 or later
* Protoc v3.4 or later

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

## License
PulseHA source code is available under the AGPL License which can be found in the LICENSE file.

Copyright (c) 2017 Andrew Zak <<andrew@pulseha.com>>
