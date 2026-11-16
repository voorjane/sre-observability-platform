DC=docker compose

all: up logs

up:
	$(DC) up -d --build

down:
	$(DC) down

logs:
	$(DC) logs -f

ps:
	$(DC) ps

clean:
	$(DC) down --remove-orphans
	docker image prune -f

test:
	cd services/order-api && go test -v -race ./...
	cd services/payment-worker && go test -v -race ./...

fmt:
	gofmt -w services/
	cd services/order-api && go vet ./...
	cd services/payment-worker && go vet ./...

.PHONY: build up down logs ps clean test fmt
