#!/bin/sh
# Builds LTS Cloud and installs it on the VPS with its systemd unit. The nginx site and
# certificate are set up once by hand, see the comments in nginx.conf.
set -eu
cd "$(dirname "$0")/.."

HOST=${HOST:-lts-vps}
VERSION=$(git describe --always --dirty)
OUT=$(mktemp -d)
trap 'rm -rf "$OUT"' EXIT

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$OUT/lts-cloud" .
scp -q "$OUT/lts-cloud" "$HOST:/usr/local/bin/lts-cloud.new"
scp -q deploy/lts-cloud.service "$HOST:/etc/systemd/system/lts-cloud.service"
ssh "$HOST" 'set -e
mv /usr/local/bin/lts-cloud.new /usr/local/bin/lts-cloud
systemctl daemon-reload
systemctl enable -q lts-cloud
systemctl restart lts-cloud
sleep 1
curl -fsS 127.0.0.1:8080/healthz'
