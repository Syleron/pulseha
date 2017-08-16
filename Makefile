.PHONEY: clean get
default: build
build: get
	 env GOOS=linux GOARCH=amd64 go build -v -o ./bin/pulse ./src/
get:
clean:
	go clean
