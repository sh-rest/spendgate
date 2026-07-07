.PHONY: up down migrate-up run test vet test-integration bench

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

# Measure gateway overhead end to end (starts Postgres/Redis, fake provider,
# gateway, then load-tests direct vs via-gateway). Writes bench/results-<date>.md.
bench:
	./bench/run.sh
