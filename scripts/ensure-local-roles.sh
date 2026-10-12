#!/bin/sh
set -eu
set -a
. ./.env
set +a
: "${RELAYDESK_APP_PASSWORD:?run make env first}"
: "${RELAYDESK_AUTH_PASSWORD:?run make env first}"
# Fresh volumes create these roles in 01-roles.sh. This only upgrades an older
# local volume without deleting its data; it is never part of production deploy.
attempt=0
until ${COMPOSE_COMMAND:-docker compose} exec -T postgres psql -U postgres -d relaydesk -Atc 'SELECT 1' >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 30 ]; then
    echo 'PostgreSQL initialization did not become ready' >&2
    exit 1
  fi
  sleep 1
done
${COMPOSE_COMMAND:-docker compose} exec -T postgres psql -v ON_ERROR_STOP=1 \
  -U postgres -d postgres -v app_password="$RELAYDESK_APP_PASSWORD" \
  -v auth_password="$RELAYDESK_AUTH_PASSWORD" <<'SQL'
SELECT format('CREATE ROLE relaydesk_app LOGIN PASSWORD %L', :'app_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='relaydesk_app') \gexec
SELECT format('CREATE ROLE relaydesk_auth LOGIN PASSWORD %L', :'auth_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='relaydesk_auth') \gexec
SQL
