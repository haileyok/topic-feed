ENV_FILE ?= $(HOME)/.config/topic-feed/env
COMPOSE  := docker compose --env-file $(ENV_FILE) -f deploy/docker-compose.yml
CH       := $(COMPOSE) exec -T clickhouse sh -c 'clickhouse-client --user topicfeed --password "$$CLICKHOUSE_PASSWORD" --database topicfeed --multiquery'

.PHONY: up down ps logs schema ch test backup install-backup label label-logs

# Run long-lived services from the main checkout (~/bluesky/topic-feed), not from a
# worktree: compose resolves ./clickhouse/config.d relative to the checkout it runs in.

up: ## Build and start local services, and wait until they're running
	$(COMPOSE) up -d --build --wait

down:
	$(COMPOSE) down

ps:
	$(COMPOSE) ps

logs:
	$(COMPOSE) logs --tail=100 -f

schema: ## Apply schema/*.sql in order (every statement is idempotent)
	@for f in $$(ls schema/*.sql | sort); do echo "applying $$f"; $(CH) < $$f || exit 1; done

ch: ## Interactive ClickHouse client
	$(COMPOSE) exec clickhouse sh -c 'clickhouse-client --user topicfeed --password "$$CLICKHOUSE_PASSWORD" --database topicfeed'

test:
	go test ./...

label: ## Start (or resume) the Jev labeling run over the labeling windows
	$(COMPOSE) --profile labeling up -d --build labeler

label-logs:
	$(COMPOSE) --profile labeling logs --tail=50 -f labeler

backup: ## Run the backup now
	deploy/backup.sh

install-backup: ## Install and start the nightly backup timer (systemd user units)
	mkdir -p $(HOME)/.config/systemd/user
	cp deploy/systemd/topic-feed-backup.service deploy/systemd/topic-feed-backup.timer $(HOME)/.config/systemd/user/
	systemctl --user daemon-reload
	systemctl --user enable --now topic-feed-backup.timer
