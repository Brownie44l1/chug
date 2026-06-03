.PHONY: run-api run-worker build migrate-up migrate-down tidy

run-api:
	go run cmd/api/main.go

run-worker:
	go run cmd/worker/main.go

build:
	go build -o bin/api cmd/api/main.go
	go build -o bin/worker cmd/worker/main.go

migrate-up:
	migrate -path migrations -database "${POSTGRES_URL}" up

migrate-down:
	migrate -path migrations -database "${POSTGRES_URL}" down

tidy:
	GOTOOLCHAIN=go1.22.2 go mod tidy
