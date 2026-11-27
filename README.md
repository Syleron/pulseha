```
   ___       __        __ _____ 
  / _ \__ __/ /__ ___ / // / _ |
 / ___/ // / (_-</ -_) _  / __ |
/_/   \_,_/_/___/\__/_//_/_/ |_|
```

PulseHA is a high availability cluster daemon for Linux. It keeps groups of floating IP
addresses up on the right node of a cluster, moves them when a node fails, and announces the
move to the network.

## Architecture Overview

PulseHA consists of two programs:

- **`pulseha`**: the daemon. One runs on every node; the daemons health-check each other,
  elect an Active node, and place the floating IPs.
- **`pulsectl`**: the CLI. It talks to the local daemon over a Unix socket and is how you
  create clusters, manage groups and inspect state.

Nodes talk to each other over gRPC on each node's bind address and port, optionally over TLS
(see [Cluster TLS](#cluster-tls)).

## Features

### High availability
- **Two modes**
  - **Active-passive:** one Active node holds every floating IP. The others are Passive and
    take over if it fails.
  - **Active-active:** every healthy node is Active and holds a share of the group's
    addresses. A failed node's share is redistributed, and nodes that recover are given
    addresses back.
- **Automatic failover.** Each node health-checks the others every `hcs_interval`. An
  Active node not heard from for `fo_limit` is failed over.
- **Automatic failback** (`auto_failback`).
- **Quorum** on clusters of three or more nodes, so a minority side of a partition cannot
  claim addresses the majority still serves. Two-node clusters are a deliberate exception;
  see [Quorum](#quorum).
- **Maintenance mode and per-node capacity** to take a node out of service, or cap how many
  addresses it may hold.

### Floating IPs
- Addresses are organised in **groups**, and a group is assigned to an interface on each node
  that may hold it.
- Moves are **announced** with gratuitous ARP (IPv4, `arping`) or unsolicited neighbour
  advertisements (IPv6, `ndptool`).
- The daemon **watches its interfaces** and restores an expected address that disappears.
- A floating IP is **never left as the primary address of its subnet**, so health checks and
  replies keep leaving from the interface's own address
  ([ADR-0006](docs/adr/0006-a-floating-ip-is-never-the-primary-address-of-its-subnet.md)).

### Security
- **Cluster join tokens.** A node can only join with the cluster's token.
- **Optional mutual TLS** between nodes, with each node's certificate pinned through the
  cluster config ([ADR-0005](docs/adr/0005-cluster-trust-is-a-config-allowlist.md)).
- The config file is written with permissions `0600`.

### Logging
- To syslog (local or remote, with a configurable facility and tag), to a file, or both.
- Four levels: `debug`, `info`, `warn`, `error`.

## Installation

PulseHA runs on Linux. The daemon must run as root, because it adds and removes addresses on
network interfaces.

### From source

```bash
git clone https://github.com/syleron/pulseha.git
cd pulseha

# rpc/*.pb.go is generated, not committed. With protoc on PATH, install the Go
# plugins once ($(go env GOPATH)/bin must be on PATH too), then generate
make tools
make protos

# Build the daemon and the CLI (both into ./cmd/*/bin/)
make build cli

# Install both into /usr/local/sbin, the systemd unit into /etc/systemd/system,
# create /etc/pulseha, and reload systemd
sudo make install
sudo systemctl enable --now pulseha
```

`make install` also supports packaging:

```bash
# Stage the install under another root. Nothing is written outside it and systemd is not
# reloaded. PREFIX moves the binaries (default /usr/local) and the unit's ExecStart follows.
make install DESTDIR=/tmp/pkgroot PREFIX=/usr
```

To build without `make`, `go build -o pulseha ./cmd/pulseha` and
`go build -o pulsectl ./cmd/pulsectl` work once `rpc/*.pb.go` has been generated.

## Configuration

The daemon keeps its configuration in a JSON file:

| Running as | Config file | CLI socket |
|---|---|---|
| root (normal) | `/etc/pulseha/config.json` | `/var/run/pulseha/pulseha.sock` |
| a user that cannot write `/etc/pulseha` | `~/.pulseha/config.json` | `~/.pulseha/pulseha.sock` |

The daemon writes a default config on first start. The cluster sections (`nodes`,
`floating_ip_groups`) are filled in by `cluster create`, `cluster join` and the `group`
commands. Change settings with `pulsectl config set` rather than by editing the file: a
setting that applies to the whole cluster is then propagated to every node.

### Example

```json
{
  "pulseha": {
    "hcs_interval": 1000,
    "fos_interval": 5000,
    "fo_limit": 10000,
    "local_node": "<this node's ID>",
    "cluster_token": "<set by cluster create>",
    "logging_level": "info",
    "auto_failback": true,
    "log_to_file": false,
    "log_file_location": "/etc/pulseha/pulseha.log",
    "log_to_syslog": true,
    "syslog_network": "",
    "syslog_address": "",
    "syslog_facility": "LOG_DAEMON",
    "syslog_tag": "pulseha",
    "mode": "active-passive",
    "nm_address_ownership": false
  },
  "floating_ip_groups": {
    "web": ["192.168.1.100/24", "192.168.1.101/24"]
  },
  "nodes": {
    "<node ID>": {
      "hostname": "node1",
      "bind_address": "192.168.1.10",
      "bind_port": "8080",
      "group_assignments": { "eth0": ["web"] }
    }
  }
}
```

### Settings

Each setting applies either to the **cluster** (`config set` propagates it to every node) or to
the **node** it is set on.

| Key | Scope | Default | Meaning |
|---|---|---|---|
| `mode` | cluster | `active-passive` | `active-passive` or `active-active`. Prefer `pulsectl cluster mode set`. |
| `hcs_interval` | cluster | `1000` | Health-check interval, ms. Minimum 1000. |
| `fo_limit` | cluster | `10000` | How long, in ms, an Active node can go unheard from before it is failed over. Minimum 1000, and not less than `fos_interval`. |
| `fos_interval` | cluster | `5000` | Failover interval, ms. Validated (minimum 1000, at most `fo_limit`) but not otherwise used by the current daemon. |
| `auto_failback` | cluster | `true` | Hand addresses back to a node that recovers. |
| `tls_mode` | cluster | `permissive` | `permissive` or `required`. See [Cluster TLS](#cluster-tls). |
| `logging_level` | node | `info` | `debug`, `info`, `warn` or `error`. |
| `log_to_syslog` | node | `true` | Log to syslog. |
| `syslog_network` | node | `""` | `""` for the local syslog, or `udp` / `tcp` for a remote one. |
| `syslog_address` | node | `""` | Remote syslog `host:port`, with `syslog_network`. |
| `syslog_facility` | node | `LOG_INFO` | One of `LOG_LOCAL0`–`LOG_LOCAL7`, `LOG_USER`, `LOG_DAEMON`, `LOG_SYSLOG`. Any other value, including the default `LOG_INFO`, which is a severity rather than a facility, logs with facility 0 (`kern`). Set one explicitly. |
| `syslog_tag` | node | `pulseha` | Syslog tag. |
| `log_to_file` | node | `false` | Also log to `log_file_location`. There is no built-in rotation; use `logrotate` with `copytruncate`. |
| `log_file_location` | node | `<config dir>/pulseha.log` | Log file path. |
| `nm_address_ownership` | node | `false` | Treat an address on a managed interface that no NetworkManager profile claims as one PulseHA placed, and release it when no group configures it. |

Per-node entries under `nodes` also carry `maintenance`, `capacity` (maximum floating IPs this
node may hold; `0` is unlimited) and `tls_cert`. Use `pulsectl node maintenance` and
`pulsectl node capacity` for the first two; the daemon manages `tls_cert` itself.

```bash
pulsectl config all              # every setting (--json for machine-readable output)
pulsectl config get fo_limit     # one setting
pulsectl config set fo_limit 15000
```

`config get` knows `mode`, `logging_level`, `log_to_file`, `log_to_syslog`, `hcs_interval`,
`fos_interval`, `fo_limit`, `auto_failback` and `nm_address_ownership`. `config set` accepts
every key in the table above.

## Usage

### The daemon

```bash
sudo systemctl enable --now pulseha
sudo systemctl status pulseha
journalctl -t pulseha -f           # follow the daemon's log
```

### Creating and joining a cluster

```bash
# On the first node: create the cluster (port defaults to 8080)
pulsectl cluster create --bind-ip 192.168.1.10
pulsectl cluster create --bind-ip 192.168.1.10 --bind-port 9443 --mode active-active

# Print the join token
pulsectl cluster token
pulsectl cluster token --regenerate   # new token, synced to every node

# On each other node: join through any existing member
pulsectl cluster join --address 192.168.1.10:8080 --token <TOKEN> --bind-ip 192.168.1.11
```

`--node-id` on `create` and `join` sets the node's ID instead of generating one, which lets a
node that leaves and rejoins keep its identity.

**A node that joins starts in maintenance mode** and takes no addresses until you assign it a
group and take it out of maintenance:

```bash
pulsectl group assign --group web --node-id <NEW NODE ID> --interface eth0
pulsectl node maintenance --disable --node-id <NEW NODE ID>
```

To remove the local node from its cluster: `pulsectl cluster leave`. To remove another node:
`pulsectl node remove --node-id <ID>`.

### Cluster mode

```bash
pulsectl status                                    # the current mode is shown at the top
pulsectl cluster mode set --mode active-active
pulsectl cluster mode set --mode active-passive    # consolidates every address onto one node
```

### Floating IP groups

```bash
pulsectl group create web
pulsectl group add-ip --group web --ip 192.168.1.100/24
pulsectl group remove-ip --group web --ip 192.168.1.100/24

# Refuse the address if another host on the segment already answers for it.
# An ARP probe from the group's interface on this node, so IPv4 only, and not
# for a network that answers ARP for every address (cloud virtual networks,
# proxy ARP): there it refuses everything.
pulsectl group add-ip --group web --ip 192.168.1.100/24 --verify-unused

# A group is held only on nodes it is assigned to, on the interface named here
pulsectl group assign   --group web --node-id <ID> --interface eth0
pulsectl group unassign --group web --node-id <ID> --interface eth0

pulsectl group list            # --json for machine-readable output
pulsectl group delete web      # --force to delete a group that is still assigned
```

Node IDs are shown by `pulsectl status`.

### Nodes

```bash
pulsectl node promote --node-id <ID>       # make this node the Active (active-passive)
pulsectl node promote --node-id <ID> --force   # even if the current Active cannot be demoted
pulsectl node demote  --node-id <ID>

pulsectl node maintenance --node-id <ID>            # take a node out of service
pulsectl node maintenance --disable --node-id <ID>  # and back in
pulsectl node capacity 50 --node-id <ID>            # cap the addresses it may hold (0 = no cap)

pulsectl cluster network resync            # re-read the node's interfaces
```

`maintenance` and `capacity` act on the local node when `--node-id` is omitted.

### Status

```bash
pulsectl status          # cluster health, mode, and every node's status, latency and addresses
pulsectl status --json
watch pulsectl status
```

A node's status is `Active`, `Passive`, `Standby` (active-active only: healthy but holding no
addresses), `Maintenance` or `Unknown` (not reachable).

### Quorum

Quorum requires a majority of the cluster to agree before a node status change or an IP
redistribution is acted on, so that a minority side of a network partition cannot claim
addresses the majority is still serving.

**There is nothing to configure.** Quorum is automatic and derived from the cluster size:
a majority is `(N/2)+1`, recalculated as nodes join and leave. There is no `pulsectl quorum`
command and no quorum key in `config.json`.

| Cluster size | Majority | Behaviour |
|---|---|---|
| 2 | n/a | No majority exists. See below |
| 3 | 2 | Quorum voting active |
| 4 | 3 | Quorum voting active |
| 5 | 3 | Quorum voting active |

**Two-node clusters are a deliberate exception.** Two nodes give the question "am I on the
surviving side" no answer to find, so quorum is not applied below three nodes. If the two
members lose contact with each other while both remain healthy, **both will bring up the
floating IPs**. This is intended: it guarantees the addresses are never held by nobody, at the
cost of their being held twice. Note that this does not give you two serving paths — clients
and switches keep one MAC per address — so the gain is continuity, not capacity.

If you need a single-owner guarantee, run three or more nodes. See
[ADR-0002](docs/adr/0002-two-node-availability-over-safety.md) for the full reasoning and the
options that were rejected.

### Cluster TLS

Each node generates a certificate at start and publishes it into the cluster config.
`tls_mode` decides whether nodes require TLS from each other:

- **`permissive`** (default): connections between nodes stay plaintext, while every node
  publishes its certificate into the cluster config.
- **`required`**: every connection between nodes is mutually authenticated against the
  certificates in the cluster config.

Switch with `pulsectl config set tls_mode required`. The daemon refuses the switch until every
node is reachable and has published a certificate, because a node that requires TLS cannot talk
to one that does not serve it. The certificates are kept in `~/.pulseha/certs`, which for the
daemon running as root is `/root/.pulseha/certs`.

## Logging

```json
{ "pulseha": { "log_to_syslog": true, "syslog_facility": "LOG_DAEMON", "syslog_tag": "pulseha" } }
```

Remote syslog:

```json
{
  "pulseha": {
    "log_to_syslog": true,
    "syslog_network": "udp",
    "syslog_address": "syslog.example.com:514",
    "syslog_facility": "LOG_LOCAL1",
    "syslog_tag": "pulseha-production"
  }
}
```

To a file as well:

```json
{ "pulseha": { "log_to_file": true, "log_file_location": "/var/log/pulseha/pulseha.log" } }
```

All of these are node settings, so set them on each node:
`pulsectl config set syslog_facility LOG_DAEMON`.

## Tuning

Detection is driven by `hcs_interval` (how often nodes check each other) and `fo_limit` (how
long an Active node may be unheard from). Both have a floor of 1000 ms. A faster but still
valid setting:

```bash
pulsectl config set hcs_interval 1000
pulsectl config set fo_limit 3000
```

Lowering `fo_limit` makes failover quicker and also makes a busy or briefly unreachable Active
more likely to be failed over unnecessarily.

## Failover

### Health checking

1. Every node checks every other node each `hcs_interval`, with a TCP connection to its bind
   port.
2. Every fifth check, it makes a gRPC health check that confirms the peer is in the same
   cluster and reports its role. In active-passive it also does this straight away for a
   reachable peer whose role it does not yet know.
3. A peer that fails a check is marked `Unknown`. An Active node unheard from for `fo_limit`
   is failed over.

### Active-passive

1. When the Active fails, the election coordinator picks a Passive node to promote. On three
   or more nodes the promotion needs a majority vote.
2. The promoted node brings up every floating IP of the groups assigned to it, then announces
   them.
3. A node that becomes Passive releases every floating IP it holds.

### Active-active

1. When a node fails, only its addresses are redistributed, to the healthy nodes the group is
   assigned to, respecting `capacity`.
2. A node that recovers, or joins, is given addresses back as the coordinator rebalances.

### Two-Node Clusters and the Witness

A two-node cluster that loses the link between its nodes serves from both rather than risk
serving from neither. A third box joined as a **Witness** ends that when it can see which node
should keep the addresses. See [docs/witness.md](docs/witness.md) for what it does, the
topology it needs, and how to set it up.

## Testing

```bash
make test                    # unit tests: internal, cmd, packages, tests/unit
make testrace                # the same under -race (run it on Linux: see below)
make integration-test        # tests/integration
go test -v ./tests/integration/... -run Syslog
```

Some code is Linux-only (`//go:build linux`), so a race run on macOS does not compile it. Run
`make testrace` on Linux, or in a container:

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.26 make testrace
```

A local three-node cluster for manual testing:

```bash
cd docker/test
./start-cluster.sh
```

## Deployment

### systemd

`make install` installs [`pulseha.service`](pulseha.service). It runs `/usr/local/sbin/pulseha`
as root, restarts it on failure, and logs to the journal (`journalctl -t pulseha`).

## Troubleshooting

**`pulsectl` says it cannot reach the daemon.** Check `systemctl status pulseha`. The
message names the socket it tried.

**A node does not join or does not appear.**
```bash
journalctl -t pulseha -f
pulsectl status                   # is the node listed? is it Unknown?
nc -zv <node-ip> <bind-port>      # can this node reach the other's bind port?
```
A node that has just joined is in maintenance mode until you take it out (see
[Creating and joining a cluster](#creating-and-joining-a-cluster)).

**Addresses are not where you expect.**
```bash
pulsectl status          # which node is Active, and what each node holds
pulsectl group list      # are the groups assigned to the right interfaces?
ip addr show
```

**Nothing reaches syslog.** Check `syslog_facility` is one of the values listed under
[Settings](#settings), then `grep pulseha /var/log/syslog` and `systemctl status rsyslog`.

**More detail:** `pulsectl config set logging_level debug` on the node in question.

## Development

### Project structure

```
pulseha/
├── cmd/
│   ├── pulseha/          # the daemon
│   └── pulsectl/         # the CLI
├── internal/
│   ├── cli/              # pulsectl's commands
│   ├── client/           # gRPC client (CLI socket and peers)
│   ├── clustertls/       # inter-node TLS credentials
│   ├── ipam/             # placement of addresses across nodes
│   ├── membership/       # members, health checks, elections, the IP monitor
│   ├── quorum/           # quorum voting
│   └── server/           # the daemon's gRPC server and orchestration
├── packages/
│   ├── config/           # configuration file and validation
│   ├── network/          # interface address and announcement operations
│   ├── pulselock/        # instrumented mutexes (ADR-0003)
│   ├── security/         # certificate generation
│   └── ...               # client, jsonHelper, language, osStat, utils
├── rpc/                  # protobuf definitions (generated code is not committed)
├── tests/                # unit, integration, testutils
├── docker/test/          # Docker test cluster and rig scripts
└── docs/                 # ADRs, the test plan, design notes
```

Design decisions are recorded in [`docs/adr`](docs/adr). The cluster test plan, and the record
of every defect it has found, is [`docs/TEST-PLAN.md`](docs/TEST-PLAN.md).

### Contributing

1. Fork the repository and create a branch from `dev`.
2. Make your change, with tests.
3. Run `make test`, and `make testrace` on Linux.
4. Open a pull request against `dev`.

## License

This project is licensed under the GNU Affero General Public License v3.0 - see the
[LICENSE](LICENSE) file for details.

## Support

- **Issues**: [GitHub Issues](https://github.com/syleron/pulseha/issues)
- **Discussions**: [GitHub Discussions](https://github.com/syleron/pulseha/discussions)
