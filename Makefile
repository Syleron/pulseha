.PHONEY: clean get

VERSION=`git describe --tags`
BUILD=`git rev-parse HEAD`
LDFLAGS=-ldflags "-X main.Version=${VERSION} -X main.Build=${BUILD}"

default: all

all: build cli

build: get
	 env GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -v -o ./cmd/pulseha/bin/pulseha ./cmd/pulseha
buildrace: get
	 env GOOS=linux GOARCH=amd64 go build -race ${LDFLAGS} -v -o ./cmd/pulseha/bin/pulseha ./cmd/pulseha
netcore: get
	 env GOOS=linux GOARCH=amd64 go build -buildmode=plugin -o ./plugins/netcore/bin/networking.so ./plugins/netcore
hcping: get
	 env GOOS=linux GOARCH=amd64 go build -buildmode=plugin -o ./plugins/hcPing/bin/hcping.so ./plugins/hcPing
hcserial: get
	 env GOOS=linux GOARCH=amd64 go build -buildmode=plugin -o ./plugins/hcSerial/bin/hcserial.so ./plugins/hcSerial
genemailalerts: get
	 env GOOS=linux GOARCH=amd64 go build -buildmode=plugin -o ./plugins/genEmailAlerts/bin/genemail.so ./plugins/genEmailAlerts
get:
	 go mod download
	 go get -u github.com/golang/protobuf/protoc-gen-go
cli: get
	 env GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -v -o ./cmd/pulsectl/bin/pulsectl ./cmd/pulsectl
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
	cp ./cmd/pulsectl/bin/pulsectl /usr/local/sbin/
	#chmod +x /etc/pulsectl/pulse
	if [ ! -d "/etc/pulseha/" ]; then mkdir /etc/pulseha/; fi
	if [ ! -d "/usr/local/lib/pulseha" ]; then mkdir /usr/local/lib/pulseha; fi
	cp pulseha.service /etc/systemd/system/
	systemctl daemon-reload
install-netcore:
	 cp ./plugins/netcore/bin/networking.so /usr/local/lib/pulseha
install-hcping:
	 cp ./plugins/hcPing/bin/hcping.so /usr/local/lib/pulseha
install-hcserial:
	 cp ./plugins/hcSerial/bin/hcserial.so /usr/local/lib/pulseha
install-genemailalerts:
	 cp ./plugins/genEmailAlerts/bin/genemail.so /usr/local/lib/pulseha
