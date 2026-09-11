#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
set -a; source .env; set +a

COMPOSE="docker compose -f docker-compose.yml"

for f in migrations/*.sql; do
  echo "Applying $f..."
  sed \
    -e "s/__ORCHESTRATOR_APP_PASSWORD__/${ORCHESTRATOR_APP_DB_PASSWORD}/g" \
    -e "s/__ORCHESTRATOR_AUDIT_WRITER_PASSWORD__/${ORCHESTRATOR_AUDIT_WRITER_DB_PASSWORD}/g" \
    "$f" | $COMPOSE exec -T postgres env PGPASSWORD="${POSTGRES_SUPERUSER_PASSWORD}" \
    psql -v ON_ERROR_STOP=1 -U postgres -d orchestrator
done

echo "Migrations applied."
