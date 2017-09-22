.PHONEY: clean get

VERSION=`git describe`
BUILD=`git rev-parse HEAD`
LDFLAGS=-ldflags "-X main.Version=${VERSION} -X main.Build=${BUILD}"

default: all

all: build cli

build: get test
	 if [ ! -d "./bin/" ]; then mkdir ./bin/; fi
	 cp config.json ./bin/
	 env GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -v -o ./bin/pulse ./src/
macbuild: get test
	if [ ! -d "./bin/" ]; then mkdir ./bin/; fi
	cp config.json ./bin/
	env GOOS=darwin GOARCH=amd64 go build ${LDFLAGS} -v -o ./bin/pulse ./src/
get:
	 go get -d ./src/
	 go get -d ./cmd/
cli: get testCMD
	 if [ ! -d "./bin/" ]; then mkdir ./bin/; fi
	 env GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -v -o ./bin/pulseha ./cmd/
maccli: get testCMD
	 if [ ! -d "./bin/" ]; then mkdir ./bin/; fi
	 env GOOS=darwin GOARCH=amd64 go build ${LDFLAGS} -v -o ./bin/pulseha ./cmd/
protos:
	 protoc ./proto/pulse.proto --go_out=plugins=grpc:.
testCMD:
	 go test -timeout 10s -v ./cmd/
test:
	 go test -timeout 10s -v ./src/
clean:
	go clean
install: build cli
ifneq ($(shell uname),Linux)
	echo "Install only available on Linux"
	exit 1
endif
	cp ./bin/pulseha /usr/local/bin/
	if [ ! -d "/etc/pulseha/" ]; then mkdir /etc/pulseha/; fi
	cp config.json /etc/pulseha/
	cp ./bin/pulse /etc/pulseha/
	chmod +x /etc/pulseha/pulse
	cp pulseha.service /etc/systemd/system/
	systemctl daemon-reload
