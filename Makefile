BREW_PREFIX  ?= $(shell brew --prefix)
DATABASE_URL ?= "postgres://$(USER)@localhost/apollo_test?sslmode=disable"
APOLLO_IMAGE ?= apollo-backend:local
COMPOSE      = APOLLO_IMAGE=$(APOLLO_IMAGE) docker compose --env-file .env.docker

test:
	@DATABASE_URL=$(DATABASE_URL) go test -race -timeout 1s ./...

test-setup: $(BREW_PREFIX)/bin/migrate
	migrate -path migrations/ -database $(DATABASE_URL) up

build:
	@go build ./cmd/apollo

lint:
	@golangci-lint run

$(BREW_PREFIX)/bin/migrate:
	@brew install golang-migrate

docker-build:
	docker build --pull --tag $(APOLLO_IMAGE) .

# These targets are for local development only. Production uses the
# digest-pinned image and scripts/deploy-cloudflare.sh.
docker-up: docker-build
	$(COMPOSE) up -d --no-build

# Also start the self-hosted Bark relay for free-sideload notification
# delivery (see README "Bark transport").
docker-up-bark: docker-build
	$(COMPOSE) --profile bark up -d --no-build

# --profile bark so a bark-server started via docker-up-bark is torn down
# too; harmless when it was never started.
docker-down:
	$(COMPOSE) --profile bark down

docker-logs:
	$(COMPOSE) logs -f --tail=100

docker-migrate:
	$(COMPOSE) run --rm migrate

docker-psql:
	$(COMPOSE) exec postgres psql -U apollo apollo

docker-nuke:
	$(COMPOSE) --profile bark down -v

.PHONY: all build deps lint test docker-build docker-up docker-up-bark docker-down docker-logs docker-migrate docker-psql docker-nuke
