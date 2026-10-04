SHELL    := bash
.SHELLFLAGS := -eo pipefail -c
ENV_FILE ?= $(HOME)/.config/topic-feed/env

# Training data (plan §11; the current model's training flow is in docs/training.md). Override on
# the command line, e.g.
#   make export LABEL_CONFIG=5697660f73fc EXPORT=/data/exports/v1-full
TAXONOMY     ?= taxonomy/v2.1.yaml
LABEL_CONFIG ?= 5697660f73fc
FULL_CONTEXT ?=
EXPORT       ?= /data/exports/v1-latest
COMPOSE  := docker compose --env-file $(ENV_FILE) -f deploy/docker-compose.yml
CH       := $(COMPOSE) exec -T clickhouse sh -c 'clickhouse-client --user topicfeed --password "$$CLICKHOUSE_PASSWORD" --database topicfeed --multiquery'

.PHONY: up down ps logs schema ch test backup install-backup label label-logs export install-classifier classifier-logs feeds feeds-logs feeds-publish feeds-welcome profile-web images-resolve images-fetch images-purge images-stats

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

feeds-welcome: ## Post the welcome message personal feeds show while a viewer's feed is built, as the owner, dated 90 days back (DRY=1 to only print; TEXT="..."; DAYS_AGO=n; CODE=<emailed code>)
	$(COMPOSE) --profile feeds run --rm --build feedgen welcome $(if $(DRY),-dry-run,) $(if $(CODE),-code $(CODE),) $(if $(TEXT),-text "$(TEXT)",) $(if $(DAYS_AGO),-days-ago $(DAYS_AGO),)

profile-web: ## Serve a page of what a viewer's likes say they're into, on the local network at :8720 (ADDR=127.0.0.1:8720 to keep it local; ACTOR=<handle or DID> to start from)
	set -a; . $(ENV_FILE); set +a; go run ./cmd/profile -serve $(or $(ADDR),:8720) -feed for-you $(if $(ACTOR),-did $(ACTOR),)

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

# Image archive (internal/imagearchive, cmd/images): download once and keep the pictures
# of every post Jev labeled, so relabeling and training read the same pixels. Resumable.
images-resolve: ## Ask the AppView where each labeled post's pictures are (RPS=N; LIMIT=N for a pilot)
	set -a; . $(ENV_FILE); set +a; go run ./cmd/images resolve $(if $(LIMIT),-limit $(LIMIT),) $(if $(RPS),-rps $(RPS),)

images-fetch: ## Download the pending pictures into /data/images (FETCH_RPS=N WORKERS=N LIMIT=N)
	set -a; . $(ENV_FILE); set +a; go run ./cmd/images fetch $(if $(LIMIT),-limit $(LIMIT),) $(if $(FETCH_RPS),-fetch-rps $(FETCH_RPS),) $(if $(WORKERS),-workers $(WORKERS),)

images-purge: ## Mark deleted posts' pictures purged and delete files no other post needs
	set -a; . $(ENV_FILE); set +a; go run ./cmd/images purge

images-stats: ## Print posts by outcome and pictures by status/kind/policy
	set -a; . $(ENV_FILE); set +a; go run ./cmd/images stats
