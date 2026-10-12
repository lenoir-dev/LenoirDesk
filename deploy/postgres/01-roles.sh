#!/bin/sh
set -eu
: "${RELAYDESK_DB_NAME:?}"
: "${RELAYDESK_MIGRATE_PASSWORD:?}"
: "${RELAYDESK_RUNTIME_PASSWORD:?}"
: "${RELAYDESK_APP_PASSWORD:?}"
: "${RELAYDESK_AUTH_PASSWORD:?}"
psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d postgres \
  -v db_name="$RELAYDESK_DB_NAME" \
  -v migration_password="$RELAYDESK_MIGRATE_PASSWORD" \
  -v runtime_password="$RELAYDESK_RUNTIME_PASSWORD" \
  -v app_password="$RELAYDESK_APP_PASSWORD" \
  -v auth_password="$RELAYDESK_AUTH_PASSWORD" <<'SQL'
SELECT format('CREATE ROLE relaydesk_migrate LOGIN PASSWORD %L', :'migration_password') \gexec
SELECT format('CREATE ROLE relaydesk_runtime LOGIN PASSWORD %L', :'runtime_password') \gexec
SELECT format('CREATE ROLE relaydesk_app LOGIN PASSWORD %L', :'app_password') \gexec
SELECT format('CREATE ROLE relaydesk_auth LOGIN PASSWORD %L', :'auth_password') \gexec
SELECT format('CREATE DATABASE %I OWNER relaydesk_migrate', :'db_name') \gexec
SQL
