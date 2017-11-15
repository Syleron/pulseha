.PHONEY: clean get

VERSION=`git describe`
BUILD=`git rev-parse HEAD`
LDFLAGS=-ldflags "-X main.Version=${VERSION} -X main.Build=${BUILD}"

default: all

all: build cli

build: get
	 if [ ! -d "./bin/" ]; then mkdir ./bin/; fi
	 cp config.json ./bin/
	 env GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -v -o ./bin/pulse ./src/
buildrace: get
	 if [ ! -d "./bin/" ]; then mkdir ./bin/; fi
	 cp config.json ./bin/
	 env GOOS=linux GOARCH=amd64 go build -race ${LDFLAGS} -v -o ./bin/pulse ./src/
macbuild: get
	if [ ! -d "./bin/" ]; then mkdir ./bin/; fi
	cp config.json ./bin/
	env GOOS=darwin GOARCH=amd64 go build ${LDFLAGS} -v -o ./bin/pulse ./src/
get:
	 go get -d ./src/
	 go get -d ./cmd/
cli: get 
	 if [ ! -d "./bin/" ]; then mkdir ./bin/; fi
	 env GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -v -o ./bin/pulseha ./cmd/
maccli: get 
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
install: 
ifneq ($(shell uname),Linux)
	echo "Install only available on Linux"
	exit 1
endif
	cp ./bin/pulseha /usr/local/sbin/
	cp ./bin/pulse /usr/local/sbin/
	#chmod +x /etc/pulseha/pulse
	if [ ! -d "/etc/pulseha/" ]; then mkdir /etc/pulseha/; fi
	if [ ! -d "/usr/local/lib/pulseha" ]; then mkdir /usr/local/lib/pulseha; fi
	cp config.json /etc/pulseha/
	cp pulseha.service /etc/systemd/system/
	systemctl daemon-reload
