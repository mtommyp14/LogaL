.PHONY: build run migrate test docker clean tidy

BINARY_NAME=logal
MIGRATE_NAME=logal-migrate
DOCKER_TAG?=your-registry/logal:latest

build:
	go build -ldflags="-s -w" -o $(BINARY_NAME) .
	go build -ldflags="-s -w" -o $(MIGRATE_NAME) ./cmd/migrate

run:
	go run .

migrate:
	go run ./cmd/migrate

test:
	go test ./...

docker:
	docker build -t $(DOCKER_TAG) .

tidy:
	go mod tidy

clean:
	rm -f $(BINARY_NAME) $(MIGRATE_NAME)

.DEFAULT_GOAL := build
