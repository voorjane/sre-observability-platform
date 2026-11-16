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

.PHONY: build up down logs ps clean
