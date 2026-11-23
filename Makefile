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

check-prometheus:
	$(DC) run --rm --no-deps --entrypoint promtool prometheus check config /etc/prometheus/prometheus.yml
	$(DC) run --rm --no-deps --entrypoint /bin/sh prometheus -ec 'promtool test rules /etc/prometheus/tests/*.test.yml'

check-logs:
	$(DC) run --rm --no-deps loki -config.file=/etc/loki/loki.yml -config.expand-env=true -verify-config=true
	$(DC) run --rm --no-deps log-collector validate /etc/alloy/config.alloy

check-grafana:
	python3 scripts/check-grafana.py --compose "$(DC)"

smoke:
	python3 scripts/smoke.py --compose "$(DC)"

.PHONY: build up down logs ps clean test fmt check-prometheus check-logs check-grafana smoke
