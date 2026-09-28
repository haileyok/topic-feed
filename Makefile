SHELL    := bash
.SHELLFLAGS := -eo pipefail -c
ENV_FILE ?= $(HOME)/.config/topic-feed/env

# Training (plan §11). Override on the command line, e.g.
#   make export LABEL_CONFIG=5697660f73fc EXPORT=/data/exports/v1-full
#   make train  EXPORT=/data/exports/v1-full RUN=v1 EPOCHS=8
TAXONOMY     ?= taxonomy/v1.yaml
LABEL_CONFIG ?= 5697660f73fc
EXPORT       ?= /data/exports/v1-latest
RUN          ?= run-$(shell date -u +%Y%m%dT%H%MZ)
EPOCHS       ?= 8
PATIENCE     ?= 2
COMPOSE  := docker compose --env-file $(ENV_FILE) -f deploy/docker-compose.yml
CH       := $(COMPOSE) exec -T clickhouse sh -c 'clickhouse-client --user topicfeed --password "$$CLICKHOUSE_PASSWORD" --database topicfeed --multiquery'

.PHONY: up down ps logs schema ch test backup install-backup label label-logs export baseline train

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

export: ## Export Jev labels to a training set (one label per post, no eval posts)
	set -a; . $(ENV_FILE); set +a; go run ./cmd/export -taxonomy $(TAXONOMY) -label-configs $(LABEL_CONFIG) -out $(EXPORT)

baseline: ## Train and evaluate the embedding baseline on $(EXPORT)
	mkdir -p /data/models/baseline-$(RUN)
	cd trainer && uv run python baseline.py --export $(EXPORT) --taxonomy ../$(TAXONOMY) --out /data/models/baseline-$(RUN) 2>&1 | tee /data/models/baseline-$(RUN)/train.log

train: ## Train the student on $(EXPORT) into /data/models/$(RUN); watch at :6006 or in the terminal
	mkdir -p /data/models/$(RUN)
	cd trainer && uv run python train.py --export $(EXPORT) --taxonomy ../$(TAXONOMY) --out /data/models/$(RUN) \
		--epochs $(EPOCHS) --patience $(PATIENCE) 2>&1 | tee /data/models/$(RUN)/train.log

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
