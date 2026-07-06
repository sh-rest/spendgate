.PHONY: up down migrate-up run test vet test-integration

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

# Needs `make up` first: exercises the multi-replica budget-enforcement test
# against real Postgres + Redis (skipped by `make test` without a live DB).
test-integration:
	TEST_DATABASE_URL="postgres://spendgate:spendgate@localhost:5432/spendgate?sslmode=disable" \
	TEST_REDIS_URL="redis://localhost:6379" \
	go test ./internal/integration/... -run TestBudgetEnforcedAcrossReplicas -v
