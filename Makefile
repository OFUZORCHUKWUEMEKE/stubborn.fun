.PHONY: run build test test-ledger mongo-rs mongo-down tidy vet

run:
	go run ./cmd/api

build:
	go build ./...

vet:
	go vet ./...

tidy:
	go mod tidy

# Full test suite. Ledger (and later market/stake/settle) gate tests need a
# local Mongo replica set — run `make mongo-rs` first, or they skip.
test:
	STUBBORN_TEST_MONGO_URI="mongodb://localhost:27017/?replicaSet=rs0" go test ./... -race -count=1

test-ledger:
	STUBBORN_TEST_MONGO_URI="mongodb://localhost:27017/?replicaSet=rs0" go test ./internal/ledger/... -race -count=1 -v

# Brings up a single-node Mongo replica set (required for multi-document
# transactions) and waits for it to finish electing a primary.
mongo-rs:
	docker compose up -d mongo
	@echo "waiting for rs0 primary..."
	@until docker compose exec -T mongo mongosh --quiet --eval "rs.status().myState === 1" 2>/dev/null | grep -q true; do sleep 1; done
	@echo "mongo rs0 ready on mongodb://localhost:27017/?replicaSet=rs0"

mongo-down:
	docker compose down -v
