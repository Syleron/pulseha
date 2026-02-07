```
   ___       __        __ _____ 
  / _ \__ __/ /__ ___ / // / _ |
 / ___/ // / (_-</ -_) _  / __ |
/_/   \_,_/_/___/\__/_//_/_/ |_|
```

PulseHA is a powerful high availability cluster management system designed to provide automated failover and IP address management across multiple nodes in a cluster. It offers both daemon and CLI tools for comprehensive cluster management.

## Architecture Overview

PulseHA consists of two main components:

- **`pulseha`** - The main high availability daemon that runs on each cluster node
- **`pulsectl`** - The dedicated CLI tool for cluster management and administration

## Why PulseHA

PulseHA attempts to solve high availability with a more simple approach but without restricting functionality with the use of additional custom plugins. It provides enterprise-grade HA capabilities with an intuitive interface and robust testing framework.

## Features

### Core High Availability
- **Multiple Operating Modes**
  - Active-Passive: Traditional HA setup with one active node and multiple passive backups
  - Active-Active: Distributed load across multiple active nodes with intelligent IP distribution

- **Automatic Failover**
  - Health monitoring of cluster nodes with configurable intervals
  - Automatic IP redistribution on node failure
  - Configurable failover thresholds and recovery settings
  - Auto-failback capability when failed nodes recover

### IP and Network Management
- **Floating IP Group Management**
  - Dynamic IP distribution based on node capacity and health
  - Graceful IP failover and failback with minimal downtime
  - GARP (Gratuitous ARP) support for immediate network updates
  - Support for IPv4 and IPv6 addressing

### Security and Communication
- **Secure Cluster Communication**
  - TLS encryption for all inter-node communication
  - Certificate-based node authentication
  - Secure cluster join tokens with expiration
  - Role-based access control for cluster operations

### Monitoring and Observability
- **Comprehensive Logging**
  - Configurable syslog integration with facility and tag support
  - File-based logging with rotation support
  - Multiple log levels (debug, info, warn, error)
  - Structured logging for easy parsing and analysis

- **Real-time Status Monitoring**
  - Cluster health monitoring with detailed node status
  - Latency measurements and performance tracking
  - JSON and human-readable output formats
  - Webhook notifications for status changes

### Advanced Features
- **Quorum-Based Decision Making**
  - Prevents split-brain scenarios in network partitions
  - Configurable quorum policies (majority or fixed count)
  - Automatic leader election and consensus protocols

- **Plugin Architecture**
  - Extensible plugin system for custom integrations
  - Pre-built plugins for common use cases
  - API hooks for external monitoring systems

## Installation

### Binary Installation

```bash
# Clone the repository
git clone https://github.com/syleron/pulseha.git
cd pulseha

# Build the daemon
go build -o pulseha cmd/pulseha/main.go

# Build the CLI tool
go build -o pulsectl cmd/pulsectl/main.go

# Install binaries (optional)
sudo cp pulseha /usr/local/bin/
sudo cp pulsectl /usr/local/bin/
```

### Docker Installation

```bash
# Pull the official Docker image
docker pull syleron/pulseha:latest

# Or build from source
docker build -t pulseha .
```

## Configuration

PulseHA uses a JSON configuration file stored by default in:
- Production: `/etc/pulseha/config.json`
- Development: `~/.pulseha/config.json`

### Core Configuration Options

```json
{
  "pulseha": {
    "hcs_interval": 1000,
    "fos_interval": 5000,
    "fo_limit": 10000,
    "local_node": "",
    "cluster_token": "",
    "logging_level": "info",
    "auto_failback": true,
    "log_to_file": true,
    "log_file_location": "/etc/pulseha/pulseha.log",
    "log_to_syslog": true,
    "syslog_network": "",
    "syslog_address": "",
    "syslog_facility": "LOG_LOCAL0",
    "syslog_tag": "pulseha",
    "mode": "active-passive",
    "quorum_enabled": false,
    "quorum_min_nodes": 2,
    "quorum_majority": true
  },
  "floating_ip_groups": {},
  "nodes": {},
  "plugins": {}
}
```

### Configuration Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `hcs_interval` | Health check interval in milliseconds | 1000 |
| `fos_interval` | Failover interval in milliseconds | 5000 |
| `fo_limit` | Failure threshold before triggering failover | 10000 |
| `logging_level` | Log level (debug, info, warn, error) | info |
| `log_to_syslog` | Enable syslog logging | true |
| `syslog_facility` | Syslog facility (LOG_LOCAL0-7, LOG_USER, etc.) | LOG_LOCAL0 |
| `mode` | Cluster mode (active-passive, active-active) | active-passive |
| `quorum_enabled` | Enable quorum-based decisions | false |

> **Security Note:** Configuration files are written with permissions `0600` to restrict access to the owner only.

## Usage

### Daemon Management

```bash
# Start the PulseHA daemon
sudo pulseha

# Start as systemd service
sudo systemctl start pulseha
sudo systemctl enable pulseha

# Check daemon status
sudo systemctl status pulseha
```

### Cluster Operations

#### Creating a Cluster

```bash
# Create a new cluster in active-passive mode
pulsectl cluster create --bind-ip 192.168.1.10

# Create cluster in active-active mode
pulsectl cluster create --bind-ip 192.168.1.10 --active-active

# Create cluster with custom port
pulsectl cluster create --bind-ip 192.168.1.10 --bind-port 8080
```

#### Joining a Cluster

```bash
# Join an existing cluster using hostname
pulsectl cluster join --address node1.example.com --token <CLUSTER_TOKEN>

# Join using IP address and port
pulsectl cluster join --address 192.168.1.10:8080 --token <CLUSTER_TOKEN>

# Join with custom local bind settings
pulsectl cluster join --address node1.example.com --token <TOKEN> --bind-ip 192.168.1.11 --bind-port 8080
```

#### Managing Cluster Mode

```bash
# View current cluster mode
pulsectl cluster mode

# Set cluster to active-passive mode
pulsectl cluster mode set --mode active-passive

# Set cluster to active-active mode
pulsectl cluster mode set --mode active-active
```

### IP Group Management

#### Creating and Managing Groups

```bash
# Create a new IP group
pulsectl group create --name web-servers

# List all IP groups
pulsectl group list

# List groups in JSON format
pulsectl group list --json

# Delete a group
pulsectl group delete --name web-servers
```

#### IP Assignment

```bash
# Add IP addresses to a group
pulsectl group add-ip --group web-servers --ip 192.168.1.100
pulsectl group add-ip --group web-servers --ip 192.168.1.101

# Remove IP from group
pulsectl group remove-ip --group web-servers --ip 192.168.1.100

# Assign group to a node's interface
pulsectl group assign --group web-servers --node node1 --interface eth0

# Unassign group from node
pulsectl group unassign --group web-servers --node node1
```

### Monitoring and Status

#### Cluster Status

```bash
# View detailed cluster status
pulsectl status

# View status in JSON format
pulsectl status --json

# Monitor status continuously
watch pulsectl status
```

#### Node Management

```bash
# List all nodes
pulsectl node list

# View specific node details
pulsectl node show --hostname node1.example.com

# Promote node to active (active-passive mode)
pulsectl node promote --hostname node1.example.com

# Remove node from cluster
pulsectl node remove --hostname node1.example.com
```

### Quorum Management

Quorum prevents split-brain scenarios by requiring a minimum number of nodes to agree before making cluster decisions.

```bash
# Enable quorum with majority rule (recommended)
# Requires (N/2)+1 nodes - automatically adjusts as nodes join/leave
# Example: 3 nodes = need 2, 5 nodes = need 3, 7 nodes = need 4
pulsectl quorum enable --majority

# Enable quorum with fixed count (advanced)
# Requires exactly the specified number of nodes
# Example: Always need exactly 2 nodes regardless of cluster size
pulsectl quorum enable --min-nodes 2

# Disable quorum (not recommended for production)
pulsectl quorum disable

# View current quorum status
pulsectl quorum status
```

**Majority vs Fixed Count:**
- **Majority Rule**: Dynamic quorum that scales with cluster size - safer for most deployments
- **Fixed Count**: Static quorum useful for specific network architectures or testing scenarios

## CLI Tool

PulseHA uses **`pulsectl`** as the dedicated command-line interface for all cluster management operations:

```bash
# All cluster operations use pulsectl
pulsectl status
pulsectl cluster create --bind-ip 192.168.1.10
pulsectl group list
pulsectl node promote --hostname node1
```

This provides a clean separation between:
- **`pulseha`** - The daemon service (runs continuously)
- **`pulsectl`** - The management CLI tool (for administration)

## Logging Configuration

PulseHA supports multiple logging destinations with flexible configuration:

### Syslog Integration
```json
{
  "pulseha": {
    "log_to_syslog": true,
    "syslog_network": "",
    "syslog_address": "",
    "syslog_facility": "LOG_LOCAL0",
    "syslog_tag": "pulseha"
  }
}
```

### Remote Syslog
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

### File Logging
```json
{
  "pulseha": {
    "log_to_file": true,
    "log_file_location": "/var/log/pulseha/pulseha.log"
  }
}
```

## Advanced Configuration

### Quorum Configuration

For clusters with 3+ nodes, enable quorum to prevent split-brain scenarios:

```json
{
  "pulseha": {
    "quorum_enabled": true,
    "quorum_min_nodes": 2,
    "quorum_majority": true
  }
}
```

### Performance Tuning

```json
{
  "pulseha": {
    "hcs_interval": 500,
    "fos_interval": 2000,
    "fo_limit": 5000
  }
}
```

## Failover Logic

### Health Checking Process

1. **Regular Health Checks**: Each node performs health checks on all other nodes at configurable intervals
2. **Connection Verification**: TCP connectivity tests ensure nodes are reachable
3. **Latency Measurement**: Response times are tracked for performance monitoring
4. **IP Verification**: Floating IPs are tested for proper functionality

### Active-Passive Failover

1. **Primary Failure**: When the active node fails, all floating IPs move to a standby node
2. **Automatic Promotion**: The standby node becomes active and assumes all IP addresses
3. **GARP Announcements**: Network switches are updated via Gratuitous ARP
4. **Service Continuity**: Applications continue with minimal interruption

### Active-Active Failover

1. **Partial Failure**: Only affected IPs are redistributed when a node fails
2. **Load Balancing**: IPs are distributed across remaining healthy nodes
3. **Gradual Recovery**: Failed nodes gradually receive IPs back when recovering

## Testing

PulseHA includes comprehensive testing capabilities:

### Unit and Integration Tests
```bash
# Run all tests
go test ./...

# Run specific test suites
go test ./tests/unit/...
go test ./tests/integration/...

# Run with verbose output
go test -v ./tests/integration/syslog_logging_test.go
```

### Docker Test Environment
```bash
# Start the local 3-node Docker test cluster
cd docker/test
./start-cluster.sh
```

### Production Testing
```bash
# Test cluster functionality
pulsectl status
pulsectl group create --name test-group
pulsectl group add-ip --group test-group --ip 192.168.1.200
```

## Deployment Examples

### Systemd Service

Create `/etc/systemd/system/pulseha.service`:

```ini
[Unit]
Description=PulseHA High Availability Daemon
After=network.target

[Service]
Type=simple
User=pulseha
Group=pulseha
ExecStart=/usr/local/bin/pulseha
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

### Docker Compose

```yaml
version: '3.8'
services:
  pulseha-node1:
    image: syleron/pulseha:latest
    container_name: pulseha-node1
    hostname: node1
    network_mode: host
    privileged: true
    volumes:
      - /etc/pulseha:/etc/pulseha
      - /var/log/pulseha:/var/log/pulseha
    environment:
      - PULSEHA_BIND_IP=192.168.1.10
      - PULSEHA_NODE_ID=node1
```

## Troubleshooting

### Common Issues

**Cluster Formation Problems:**
```bash
# Check daemon logs
journalctl -u pulseha -f

# Verify network connectivity
pulsectl node list
telnet <node-ip> 8080
```

**IP Failover Issues:**
```bash
# Check IP group assignments
pulsectl group list --json

# Verify interface configuration
ip addr show
pulsectl status
```

**Syslog Not Working:**
```bash
# Check syslog configuration
grep pulseha /var/log/syslog
systemctl status rsyslog

# Test manual syslog
logger -t pulseha "Test message"
```

### Debug Mode

Enable debug logging for detailed troubleshooting:

```json
{
  "pulseha": {
    "logging_level": "debug"
  }
}
```

## Development

### Project Structure

```
pulseha/
├── cmd/
│   ├── pulseha/              # Main daemon with embedded CLI
│   └── pulsectl/             # Dedicated CLI tool
├── internal/
│   ├── cli/                  # Shared CLI commands
│   ├── client/               # gRPC client implementation
│   ├── membership/           # Cluster membership management
│   ├── server/               # gRPC server implementation
│   ├── ipam/                 # IP address management
│   └── quorum/               # Quorum and consensus logic
├── packages/
│   ├── config/               # Configuration management
│   ├── logging/              # Logging infrastructure
│   ├── network/              # Network operations
│   ├── security/             # TLS and certificate management
│   └── utils/                # Utility functions
├── rpc/                      # Protocol buffer definitions
├── tests/
│   ├── unit/                 # Unit tests
│   ├── integration/          # Integration tests
│   └── testutils/            # Test utilities and mocks
└── docker/
    └── test/                 # Docker test environment
```

### Building from Source

```bash
# Install dependencies
go mod download

# Build daemon
make build

# Build CLI
make build-cli

# Run tests
make test

# Run integration tests
make integration-test
```

### Contributing

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Make your changes
4. Add tests for new functionality
5. Ensure all tests pass (`make test`)
6. Commit your changes (`git commit -m 'Add amazing feature'`)
7. Push to the branch (`git push origin feature/amazing-feature`)
8. Open a Pull Request

## License

This project is licensed under the GNU Affero General Public License v3.0 - see the [LICENSE](LICENSE) file for details.

## Support

- **Documentation**: [GitHub Wiki](https://github.com/syleron/pulseha/wiki)
- **Issues**: [GitHub Issues](https://github.com/syleron/pulseha/issues)
- **Discussions**: [GitHub Discussions](https://github.com/syleron/pulseha/discussions)

For questions, feature requests, and contributions, please visit our [GitHub repository](https://github.com/syleron/pulseha).
