.PHONEY: clean get
default: build
build: get test
	 if [ ! -d "./bin/" ]; then mkdir ./bin/; fi
	 cp config.json ./bin/
	 env GOOS=linux GOARCH=amd64 go build -v -o ./bin/pulse ./src/
get:
	 go get -d ./src/
cli: testCMD
	 if [ ! -d "./bin/" ]; then mkdir ./bin/; fi
	 env GOOS=linux GOARCH=amd64 go build -v -o ./bin/pulseha ./cmd/
protos:
	 protoc ./proto/pulse.proto --go_out=plugins=grpc:.
install: build cli
	 # Move the cmd binary to /usr/bin
	 # Create pulseha folder in /etc/
testCMD:
	 go test -timeout 10s -v ./cmd/
test:
	 go test -timeout 10s -v ./src/
clean:
	go clean
