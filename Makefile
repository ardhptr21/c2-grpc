.PHONY: proto server agent operator

proto:
	@mkdir -p pb
	PATH="$(PATH):$(shell go env GOPATH)/bin" \
	protoc --go_out=./pb --go_opt=paths=source_relative \
	       --go-grpc_out=./pb --go-grpc_opt=paths=source_relative \
	       -I proto proto/c2.proto

server:
	go run ./server

agent:
	go run ./agent

operator:
	go run ./operator
