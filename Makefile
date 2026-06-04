.PHONY: run-api run-worker build migrate-up migrate-down tidy

include .env
export

run-api:
	GOTOOLCHAIN=go1.22.2 go run cmd/api/main.go

run-worker:
	GOTOOLCHAIN=go1.22.2 go run cmd/worker/main.go

build:
	GOTOOLCHAIN=go1.22.2 go build -o bin/api cmd/api/main.go
	GOTOOLCHAIN=go1.22.2 go build -o bin/worker cmd/worker/main.go

migrate-up:
	migrate -path migrations -database "${POSTGRES_URL}" up

migrate-down:
	migrate -path migrations -database "${POSTGRES_URL}" down

test:
	GOTOOLCHAIN=go1.22.2 go test ./... -v -count=1

tidy:
	GOTOOLCHAIN=go1.22.2 go mod tidy

run-benchmark:
	GOTOOLCHAIN=go1.22.2 go run cmd/benchmark/main.go -server "http://localhost:8080" -total 50 -concurrency 5 -dups 30