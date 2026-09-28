#!/usr/bin/env bash
# Nightly backup (plan §6). Backs up what can't be rebuilt from Jetstream:
#   - ClickHouse tables jev_labels, jev_requests, and models (via BACKUP ... TO Disk)
#   - promoted model files under /data/models
# Posts, likes, and the other ingest tables are not backed up: the Jetstream archive
# can replay them. The taxonomy and labeling-window files live in git.
#
# Backups land on the OS drive under ~/backups/topic-feed. ClickHouse backups older
# than KEEP_DAYS are removed.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
ENV_FILE="${ENV_FILE:-$HOME/.config/topic-feed/env}"
BACKUP_ROOT="${BACKUP_ROOT:-$HOME/backups/topic-feed}"
MODELS_DIR="${MODELS_DIR:-/data/models}"
KEEP_DAYS="${KEEP_DAYS:-14}"

compose=(docker compose --env-file "$ENV_FILE" -f "$REPO/deploy/docker-compose.yml")
stamp="$(date -u +%Y%m%dT%H%M%SZ)"

echo "backing up ClickHouse tables to $BACKUP_ROOT/clickhouse/$stamp"
printf "BACKUP TABLE topicfeed.jev_labels, TABLE topicfeed.jev_requests, TABLE topicfeed.models TO Disk('backups', '%s')\n" "$stamp" |
  "${compose[@]}" exec -T clickhouse sh -c 'clickhouse-client --user topicfeed --password "$CLICKHOUSE_PASSWORD"'

echo "copying model files from $MODELS_DIR"
mkdir -p "$BACKUP_ROOT/models"
rsync -a --delete "$MODELS_DIR/" "$BACKUP_ROOT/models/"

# Backup files belong to the container's clickhouse user, so prune from inside the container.
echo "removing ClickHouse backups older than $KEEP_DAYS days"
"${compose[@]}" exec -T -u 0 clickhouse \
  find /backups -mindepth 1 -maxdepth 1 -type d -mtime "+$KEEP_DAYS" -print -exec rm -rf {} +

echo "backup $stamp done"
