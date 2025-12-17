#!/usr/bin/env bash
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "[xrunner] install.sh must run as root" >&2
  exit 1
fi

if ! command -v go >/dev/null 2>&1; then
  echo "[xrunner] Go toolchain is required on the target host" >&2
  exit 1
fi

XRUNNER_USER="xrunner"
XRUNNER_GROUP="xrunner"
STATE_DIR="/var/lib/xrunner"
CONFIG_DIR="/etc/xrunner"
DEFAULTS_DIR="/etc/default"
SYSTEMD_DIR="/etc/systemd/system"
BIN_API="/usr/local/bin/xrunner-api"
BIN_WORKER="/usr/local/bin/xrunner-worker"
BIN_CLI="/usr/local/bin/xrunner"

if ! id -u "$XRUNNER_USER" >/dev/null 2>&1; then
  useradd --system --create-home --home-dir "$STATE_DIR" --shell /usr/sbin/nologin "$XRUNNER_USER"
fi

install -d -m 0755 -o "$XRUNNER_USER" -g "$XRUNNER_GROUP" "$STATE_DIR"
install -d -m 0755 -o "$XRUNNER_USER" -g "$XRUNNER_GROUP" "$STATE_DIR/workspaces"
install -d -m 0755 "$CONFIG_DIR"

# Build binaries locally and place them into /usr/local/bin
GOFLAGS=${GOFLAGS:-}
go build $GOFLAGS -o "$BIN_API" ./cmd/api
go build $GOFLAGS -o "$BIN_WORKER" ./cmd/worker
go build $GOFLAGS -o "$BIN_CLI" ./cmd/xrunner
chown root:root "$BIN_API" "$BIN_WORKER" "$BIN_CLI"
chmod 0755 "$BIN_API" "$BIN_WORKER" "$BIN_CLI"

install -m 0644 configs/default.nsjail.cfg "$CONFIG_DIR/default.nsjail.cfg"
install -m 0644 deploy/systemd/xrunner-api.env "$DEFAULTS_DIR/xrunner-api"
install -m 0644 deploy/systemd/xrunner-worker.env "$DEFAULTS_DIR/xrunner-worker"

install -m 0644 deploy/systemd/xrunner-api.service "$SYSTEMD_DIR/xrunner-api.service"
install -m 0644 deploy/systemd/xrunner-worker.service "$SYSTEMD_DIR/xrunner-worker.service"

systemctl daemon-reload
systemctl enable --now xrunner-worker.service
systemctl enable --now xrunner-api.service

systemctl status xrunner-worker.service --no-pager
systemctl status xrunner-api.service --no-pager

echo "[xrunner] deployment complete. API listening on ${XRUNNER_API_LISTEN:-0.0.0.0:50051}."
