# Must track go.mod's `go` directive (1.25.0). Pinned at 1.23 the builder failed
# every rig in this directory at `go mod download` with "go.mod requires go >= 1.25.0",
# so none of them had been buildable since that bump.
FROM golang:1.25-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git make gcc musl-dev linux-headers

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build PulseHA daemon and CLI
RUN go build -o pulseha cmd/pulseha/main.go
RUN go build -o pulsectl cmd/pulsectl/main.go

# Runtime image
FROM alpine:latest

# Install runtime dependencies
# iputils-arping supplies the announcer PulseHA shells out to for IPv4 floating IPs
# (packages/network announceCommand: `arping -U -c 5 -I <iface> <ip>`). Without it
# requireAnnouncer fails the PATH lookup and NO floating IP is ever announced, so
# every ARP cache on the bridge keeps whichever MAC it learned first. Any rig that
# measures failover reachability needs this present.
RUN apk add --no-cache \
    iproute2 \
    iptables \
    iputils-arping \
    curl \
    bash \
    net-tools \
    procps

# Create pulseha user
RUN addgroup -g 1001 pulseha && \
    adduser -D -s /bin/bash -u 1001 -G pulseha pulseha

# Create necessary directories
RUN mkdir -p /etc/pulseha /var/log/pulseha /home/pulseha/.pulseha && \
    chown -R pulseha:pulseha /etc/pulseha /var/log/pulseha /home/pulseha/.pulseha

# Copy binaries from builder
COPY --from=builder /app/pulseha /usr/local/bin/pulseha
COPY --from=builder /app/pulsectl /usr/local/bin/pulsectl

# Make binaries executable
RUN chmod +x /usr/local/bin/pulseha /usr/local/bin/pulsectl

# Announcement counter. requireAnnouncer resolves "arping" through PATH, and
# /usr/local/bin precedes /usr/sbin, so this shim sees every announcement PulseHA
# makes and records it before handing off to the real binary. Counting at this
# boundary rather than in the daemon's logs is deliberate: the claim under test is
# that a node which never brings an address up never announces it, and a log line
# can be added or moved without changing whether a frame reached the wire.
COPY docker/test/arping-shim.sh /usr/local/bin/arping
RUN chmod +x /usr/local/bin/arping

# For testing, run as root so netlink operations (addr add/del) succeed
WORKDIR /root

# Expose gRPC port
EXPOSE 8080

# Health check using pulsectl
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD /usr/local/bin/pulsectl status --json > /dev/null || exit 1

# Default command
CMD ["/usr/local/bin/pulseha"]