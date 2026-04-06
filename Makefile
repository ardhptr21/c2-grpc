.PHONY: proto server agent operator build clean

BUILD_DIR := builds

proto:
	@mkdir -p pb
	PATH="$(PATH):$(shell go env GOPATH)/bin" \
	protoc --go_out=./pb --go_opt=paths=source_relative \
	       --go-grpc_out=./pb --go-grpc_opt=paths=source_relative \
	       -I proto proto/c2.proto

build: server operator agent

server:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build -o $(BUILD_DIR)/server-linux-amd64 ./server

operator:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build -o $(BUILD_DIR)/operator-linux-amd64 ./operator

agent:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build -o $(BUILD_DIR)/agent-linux-amd64 ./agent
	GOOS=windows GOARCH=amd64 go build -o $(BUILD_DIR)/agent-windows-amd64.exe ./agent

clean:
	rm -rf $(BUILD_DIR)
