.PHONEY: clean get protos test integration-test quiet-integration-test test-all

VERSION=`git describe --tags`
BUILD=`git rev-parse HEAD`
LDFLAGS=-ldflags "-X main.Version=${VERSION} -X main.Build=${BUILD}"

default: all

all: protos build cli

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
	 go get -u google.golang.org/protobuf/cmd/protoc-gen-go
	 go get -u google.golang.org/grpc/cmd/protoc-gen-go-grpc
cli: get
	 env GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -v -o ./cmd/pulsectl/bin/pulsectl ./cmd/pulsectl
protos:
	 protoc --go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		./rpc/server.proto
test:
	 go test -timeout 30s -v ./internal/...
	 go test -timeout 30s -v ./cmd/...
	 go test -timeout 30s -v ./packages/...
testrace:
	 go test -race -timeout 30s -v ./internal/...
	 go test -race -timeout 30s -v ./cmd/...
	 go test -race -timeout 30s -v ./packages/...
integration-test:
	 @echo "Running integration tests (verbose mode)..."
	 go test -timeout 2m -v ./tests/integration/...
quiet-integration-test:
	 @echo "Running integration tests (quiet mode)..."
	 PULSEHA_TEST_LOGLEVEL=error go test -timeout 2m ./tests/integration/...
test-all: test quiet-integration-test
	 @echo "All tests completed!"
clean:
	go clean -modcache
	rm -f ./rpc/*.pb.go
	rm -f ./rpc/*/*.pb.go
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
