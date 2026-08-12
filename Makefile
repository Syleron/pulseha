# Every target here is a recipe, not a file. This MUST stay .PHONY (it read
# .PHONEY until 2026-07-28, which make treats as an ordinary target, leaving
# every rule below file-shadowable): build.sh generates PHP proto stubs into
# ./test/, and that directory silently turned `make test` into
# "make: 'test' is up to date" — a green run that executed no tests at all.
.PHONY: default all build buildrace netcore hcping hcserial genemailalerts \
	get tools cli protos test testrace integration-test \
	quiet-integration-test test-all clean install install-netcore \
	install-hcping install-hcserial install-genemailalerts

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
# Installs the protoc plugins that `protos` needs on PATH. Run once, or after
# changing the protobuf/grpc versions — deliberately not a prerequisite of
# `build`: `go install pkg@ver` is module-agnostic and leaves go.mod alone,
# whereas the `go get -u` calls this replaces rewrote go.mod and go.sum on every
# single build. Keep the versions in step with .github/workflows/{dev,master}.yml
# — rpc/*.pb.go is generated rather than committed, so a skew between a
# developer and CI surfaces as differing generated code.
tools:
	 go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.7
	 go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
cli: get
	 env GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -v -o ./cmd/pulsectl/bin/pulsectl ./cmd/pulsectl
protos:
	 protoc --go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		./rpc/server.proto
# 120s, not 30s. internal/server alone runs ~15s on a dev machine and ~20s under
# -race, and a CI runner is slower than that -- with both targets now actually
# running in CI, a 30s per-package budget was close enough to the wall to fail on
# runner load rather than on a real regression.
test:
	 go test -timeout 120s -v ./internal/...
	 go test -timeout 120s -v ./cmd/...
	 go test -timeout 120s -v ./packages/...
testrace:
	 go test -race -timeout 120s -v ./internal/...
	 go test -race -timeout 120s -v ./cmd/...
	 go test -race -timeout 120s -v ./packages/...
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
