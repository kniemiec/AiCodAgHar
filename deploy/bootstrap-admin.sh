#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
set -a; source .env; set +a

COMPOSE="docker compose -f docker-compose.yml"

echo "Waiting for Forgejo to become reachable..."
until curl -sf "${FORGEJO_URL}/api/healthz" >/dev/null 2>&1; do sleep 2; done

if ! $COMPOSE exec -u git forgejo forgejo admin user list | grep -q "^\s*[0-9]*\s*${FORGEJO_ADMIN_USERNAME}\b"; then
  $COMPOSE exec -u git forgejo forgejo admin user create \
    --username "${FORGEJO_ADMIN_USERNAME}" \
    --password "${FORGEJO_ADMIN_PASSWORD}" \
    --email "${FORGEJO_ADMIN_USERNAME}@local.invalid" \
    --admin --must-change-password=false
else
  echo "Admin user ${FORGEJO_ADMIN_USERNAME} already exists, skipping create."
fi

TOKEN_NAME="bootstrap-$(date +%s)"
TOKEN_OUTPUT=$($COMPOSE exec -u git forgejo forgejo admin user generate-access-token \
  --username "${FORGEJO_ADMIN_USERNAME}" \
  --token-name "${TOKEN_NAME}" \
  --scopes write:admin,write:repository,write:user,write:organization,write:issue)

TOKEN=$(echo "$TOKEN_OUTPUT" | grep -oE '[0-9a-f]{40}')
if [ -z "${TOKEN}" ]; then
  echo "Failed to parse token from: $TOKEN_OUTPUT" >&2
  exit 1
fi

grep -v '^FORGEJO_ADMIN_TOKEN=' .env > .env.tmp || true
mv .env.tmp .env
echo "FORGEJO_ADMIN_TOKEN=${TOKEN}" >> .env

echo "Admin token written to deploy/.env as FORGEJO_ADMIN_TOKEN."
echo "Verify with: curl -H \"Authorization: token \$FORGEJO_ADMIN_TOKEN\" ${FORGEJO_URL}/api/v1/user"

# pdp-mock and control-plane read FORGEJO_TOKEN at container-start time and
# crash-loop with no token until now; recreate them so they pick up the
# token we just minted.
for svc in pdp-mock control-plane; do
  if $COMPOSE ps --services 2>/dev/null | grep -qx "$svc"; then
    echo "Recreating $svc so it picks up the new token..."
    $COMPOSE up -d "$svc"
  fi
done
