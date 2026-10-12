#!/bin/sh
set -eu
if [ ! -e .env ]; then
pg_password=$(openssl rand -hex 24)
migrate_password=$(openssl rand -hex 24)
runtime_password=$(openssl rand -hex 24)
encryption_key=$(openssl rand -base64 32 | tr -d '\n')
umask 077
cat > .env <<EOF
RELAYDESK_POSTGRES_PASSWORD=$pg_password
RELAYDESK_MIGRATE_PASSWORD=$migrate_password
RELAYDESK_RUNTIME_PASSWORD=$runtime_password
RELAYDESK_MIGRATION_DATABASE_URL=postgres://relaydesk_migrate:$migrate_password@postgres:5432/relaydesk?sslmode=disable
RELAYDESK_DATABASE_URL=postgres://relaydesk_runtime:$runtime_password@postgres:5432/relaydesk?sslmode=disable
RELAYDESK_ENCRYPTION_KEY_B64=$encryption_key
EOF
echo 'Created .env with local development credentials' >&2
fi
if ! grep -q '^RELAYDESK_APP_DATABASE_URL=' .env; then
app_password=$(openssl rand -hex 24)
umask 077
cat >> .env <<EOF
RELAYDESK_APP_PASSWORD=$app_password
RELAYDESK_APP_DATABASE_URL=postgres://relaydesk_app:$app_password@postgres:5432/relaydesk?sslmode=disable
EOF
fi
if ! grep -q '^RELAYDESK_AUTH_DATABASE_URL=' .env; then
auth_password=$(openssl rand -hex 24)
umask 077
cat >> .env <<EOF
RELAYDESK_AUTH_PASSWORD=$auth_password
RELAYDESK_AUTH_DATABASE_URL=postgres://relaydesk_auth:$auth_password@postgres:5432/relaydesk?sslmode=disable
EOF
fi
go run scripts/dev-oidc-env.go
