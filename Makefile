.PHONEY: clean get
default: build
build: get
	 if [ ! -d "./bin/" ]; then mkdir ./bin/; fi
	 cp config.json ./bin/
	 env GOOS=linux GOARCH=amd64 go build -v -o ./bin/pulse ./src/
get:
	 go get -d ./src/
cli:
	 if [ ! -d "./bin/" ]; then mkdir ./bin/; fi
	 env GOOS=linux GOARCH=amd64 go build -v -o ./bin/pulseha ./cmd/
clean:
	go clean
