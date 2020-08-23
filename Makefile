.PHONEY: clean get

VERSION=`git describe --tags`
BUILD=`git rev-parse HEAD`
LDFLAGS=-ldflags "-X main.Version=${VERSION} -X main.Build=${BUILD}"

default: all

all: build cli

build: get
	 env GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -v -o ./cmd/pulse/bin/pulse ./cmd/pulse
buildrace: get
	 env GOOS=linux GOARCH=amd64 go build -race ${LDFLAGS} -v -o ./cmd/pulse/bin/pulse ./cmd/pulse
netcore: get
	 env GOOS=linux GOARCH=amd64 go build -buildmode=plugin -o ./plugins/netcore/bin/networking.so ./plugins/netcore
get:
	 go mod download
	 go get -u github.com/golang/protobuf/protoc-gen-go
cli: get 
	 env GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -v -o ./cmd/pulseha/bin/pulseha ./cmd/pulseha
protos:
	 protoc ./rpc/pulse.proto --go_out=plugins=grpc:.
test:
#	 go test -timeout 10s -v ./src/...
#	 go test -timeout 10s -v ./cmd/...
clean:
	go clean -modcache
install: 
ifneq ($(shell uname),Linux)
	echo "Install only available on Linux"
	exit 1
endif
	cp ./cmd/pulseha/bin/pulseha /usr/local/sbin/
	cp ./cmd/pulse/bin/pulse /usr/local/sbin/
	#chmod +x /etc/pulseha/pulse
	if [ ! -d "/etc/pulseha/" ]; then mkdir /etc/pulseha/; fi
	if [ ! -d "/usr/local/lib/pulseha" ]; then mkdir /usr/local/lib/pulseha; fi
	cp pulseha.service /etc/systemd/system/
	systemctl daemon-reload
netcore-install:
	 cp ./plugins/netcore/bin/networking.so /usr/local/lib/pulseha
