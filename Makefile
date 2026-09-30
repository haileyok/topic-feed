SHELL    := bash
.SHELLFLAGS := -eo pipefail -c
ENV_FILE ?= $(HOME)/.config/topic-feed/env

# Training (plan §11). Override on the command line, e.g.
#   make export LABEL_CONFIG=5697660f73fc EXPORT=/data/exports/v1-full
#   make train  EXPORT=/data/exports/v1-full RUN=v1 EPOCHS=8
TAXONOMY     ?= taxonomy/v1.yaml
LABEL_CONFIG ?= 5697660f73fc
FULL_CONTEXT ?=
EXPORT       ?= /data/exports/v1-latest
RUN          ?= run-$(shell date -u +%Y%m%dT%H%MZ)
EPOCHS       ?= 8
PATIENCE     ?= 2
COMPOSE  := docker compose --env-file $(ENV_FILE) -f deploy/docker-compose.yml
CH       := $(COMPOSE) exec -T clickhouse sh -c 'clickhouse-client --user topicfeed --password "$$CLICKHOUSE_PASSWORD" --database topicfeed --multiquery'

# Relabeling posts Jev was unsure about with an LLM (trainer/relabel.py), e.g.
#   make relabel RELABEL=v1-luna-le05 MAX_CONF=0.5 EXPORT=/data/exports/v1-final
#   make relabel-load RELABEL=v1-luna-le05
#   make export LABEL_CONFIG=5697660f73fc,<label_config printed by relabel> EXPORT=/data/exports/v1-luna
RELABEL  ?= v1-luna-le05
MAX_CONF ?= 0.5

.PHONY: up down ps logs schema ch test backup install-backup label label-logs export baseline train relabel relabel-load install-classifier classifier-logs feeds feeds-logs feeds-publish

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
	set -a; . $(ENV_FILE); set +a; go run ./cmd/export -taxonomy $(TAXONOMY) -label-configs $(LABEL_CONFIG) -full-context-configs "$(FULL_CONTEXT)" -out $(EXPORT)

baseline: ## Train and evaluate the embedding baseline on $(EXPORT)
	mkdir -p /data/models/baseline-$(RUN)
	cd trainer && uv run python baseline.py --export $(EXPORT) --taxonomy ../$(TAXONOMY) --out /data/models/baseline-$(RUN) 2>&1 | tee /data/models/baseline-$(RUN)/train.log

train: ## Train the student on $(EXPORT) into /data/models/$(RUN); watch at :6006 or in the terminal
	mkdir -p /data/models/$(RUN)
	cd trainer && uv run python train.py --export $(EXPORT) --taxonomy ../$(TAXONOMY) --out /data/models/$(RUN) \
		--epochs $(EPOCHS) --patience $(PATIENCE) $(TRAIN_ARGS) 2>&1 | tee /data/models/$(RUN)/train.log

relabel: ## Relabel posts in $(EXPORT) with Jev confidence <= $(MAX_CONF) using Luna (resumable)
	set -a; . $(ENV_FILE); set +a; cd trainer && uv run python relabel.py --export $(EXPORT) \
		--max-confidence $(MAX_CONF) --name $(RELABEL) 2>&1 | tee -a /data/relabel/$(RELABEL).log

relabel-load: ## Insert /data/relabel/$(RELABEL).rows.jsonl into jev_labels (rerunning replaces the same rows)
	$(COMPOSE) exec -T clickhouse sh -c 'clickhouse-client --user topicfeed --password "$$CLICKHOUSE_PASSWORD" \
		--database topicfeed --date_time_input_format best_effort -q "INSERT INTO jev_labels FORMAT JSONEachRow"' \
		< /data/relabel/$(RELABEL).rows.jsonl
	@echo "loaded $$(wc -l < /data/relabel/$(RELABEL).rows.jsonl) rows from /data/relabel/$(RELABEL).rows.jsonl"

label: ## Start (or resume) the Jev labeling run over the labeling windows
	$(COMPOSE) --profile labeling up -d --build labeler

label-logs:
	$(COMPOSE) --profile labeling logs --tail=50 -f labeler

feeds: ## Build and (re)start the feed generator, picking up config/feeds.yaml (needs FEEDGEN_HOSTNAME and FEEDGEN_OWNER_DID in the env file)
	$(COMPOSE) --profile feeds up -d --build --force-recreate --wait feedgen

feeds-logs:
	$(COMPOSE) --profile feeds logs --tail=100 -f feedgen

feeds-publish: ## Write every feed in config/feeds.yaml to the owner's account (DRY=1 to only print; CODE=<emailed code> for email 2FA)
	$(COMPOSE) --profile feeds run --rm --build feedgen publish $(if $(DRY),-dry-run,) $(if $(CODE),-code $(CODE),)

backup: ## Run the backup now
	deploy/backup.sh

install-classifier: ## Install and start the GPU classifier service (systemd user unit; model in the unit's MODEL_DIR)
	mkdir -p $(HOME)/.config/systemd/user
	cp deploy/systemd/topic-feed-classifier.service $(HOME)/.config/systemd/user/
	systemctl --user daemon-reload
	systemctl --user enable --now topic-feed-classifier.service
	systemctl --user restart topic-feed-classifier.service

classifier-logs:
	journalctl --user -u topic-feed-classifier.service -n 50 -f

install-backup: ## Install and start the nightly backup timer (systemd user units)
	mkdir -p $(HOME)/.config/systemd/user
	cp deploy/systemd/topic-feed-backup.service deploy/systemd/topic-feed-backup.timer $(HOME)/.config/systemd/user/
	systemctl --user daemon-reload
	systemctl --user enable --now topic-feed-backup.timer
