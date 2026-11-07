.PHONY: lint test build run

lint:
	golangci-lint run ./...

test:
	go test -race -cover ./...

build:
	go build -o bin/initiative-api ./cmd/initiative-api

run:
	go run ./cmd/initiative-api
