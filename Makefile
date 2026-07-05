.PHONY: up down migrate-up run test vet

up:
	docker compose up -d

down:
	docker compose down

migrate-up:
	go run ./cmd/spendgate migrate

run:
	go run ./cmd/spendgate serve

test:
	go test ./...

vet:
	go vet ./...
