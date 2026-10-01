#!/usr/bin/env bash
# Builds the Linux binary locally and (re)deploys it to the gateway host as the
# odoo-gpt-mcp container on teleaccess-net, behind the shared Caddy gateway.
#
# Usage: deploy/deploy.sh [root@178.128.216.128]
# First run creates /home/deploy/odoo-gpt-mcp/.env on the server (with a fresh OAUTH_SECRET);
# later runs keep it, so users stay signed in.
set -euo pipefail

HOST="${1:-root@178.128.216.128}"
DIR=/home/deploy/odoo-gpt-mcp
cd "$(dirname "$0")/.."

VERSION="$(git describe --always --dirty 2>/dev/null || echo dev)"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o deploy/odoo-gpt-mcp ./cmd/odoo-gpt-mcp

PLUGIN_ZIP="$(bash deploy/build-plugin.sh | tail -1)"

ssh "$HOST" "mkdir -p $DIR"
scp -q deploy/odoo-gpt-mcp deploy/Dockerfile.prebuilt "$HOST:$DIR/"
scp -q config/reports.yaml "$HOST:$DIR/reports.yaml"
scp -q "$PLUGIN_ZIP" "$HOST:$DIR/nuanu-odoo-plugin.zip"
rm -f deploy/odoo-gpt-mcp

ssh "$HOST" bash -s -- "$DIR" <<'REMOTE'
set -euo pipefail
DIR="$1"
cd "$DIR"
if [ ! -f .env ]; then
	umask 077
	cat > .env <<ENV
ODOO_URL=https://odoo.nuanu.xyz
ODOO_DB=woodenfish
MCP_PUBLIC_URL=https://odoo.mcp.nuanu.com
OAUTH_SECRET=$(openssl rand -hex 32)
REPORTS_FILE=/config/reports.yaml
ENV
	echo "created $DIR/.env with a new OAUTH_SECRET"
fi
chmod 600 .env
docker build -q -f Dockerfile.prebuilt -t odoo-gpt-mcp:latest . >/dev/null
docker rm -f odoo-gpt-mcp >/dev/null 2>&1 || true
docker run -d --name odoo-gpt-mcp \
	--network teleaccess-net \
	--restart unless-stopped \
	--env-file .env \
	-v "$DIR/reports.yaml:/config/reports.yaml:ro" \
	-v "$DIR/nuanu-odoo-plugin.zip:/plugin/nuanu-odoo-plugin.zip:ro" \
	-e PLUGIN_ZIP=/plugin/nuanu-odoo-plugin.zip \
	--memory 256m \
	odoo-gpt-mcp:latest >/dev/null
sleep 3
docker logs odoo-gpt-mcp 2>&1 | tail -5
REMOTE
