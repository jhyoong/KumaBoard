#!/usr/bin/env bash
set -euo pipefail

id kuma-agent >/dev/null 2>&1 || useradd --system --create-home --home-dir /var/lib/kuma-agent --shell /bin/bash kuma-agent

install -d -m 0755 -o kuma-agent -g kuma-agent /opt/kuma-agent
install -d -m 0755 -o root -g root /etc/kuma-agent
install -m 0644 "$(dirname "$0")/kuma-agent.service" /etc/systemd/system/kuma-agent.service
systemctl daemon-reload
systemctl enable kuma-agent

cat <<EOF
Next:
  cp kuma-agent   /opt/kuma-agent/kuma-agent && chown kuma-agent:kuma-agent /opt/kuma-agent/kuma-agent && chmod 0755 /opt/kuma-agent/kuma-agent
  cp config.yaml  /etc/kuma-agent/config.yaml   (root-owned, 0644)
  cp ca.pem       /etc/kuma-agent/ca.pem        (root-owned, 0644)
  install -m 0600 -o kuma-agent -g kuma-agent token /etc/kuma-agent/token
  systemctl start kuma-agent && journalctl -u kuma-agent -f
EOF
