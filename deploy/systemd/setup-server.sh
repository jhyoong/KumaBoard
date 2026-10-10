#!/usr/bin/env bash
set -euo pipefail

id kumaboard >/dev/null 2>&1 || useradd --system --home-dir /var/lib/kumaboard --shell /usr/sbin/nologin kumaboard
install -d -m 0755 -o root -g root /opt/kumaboard /etc/kumaboard
install -d -m 0750 -o kumaboard -g kumaboard /var/lib/kumaboard
install -m 0644 "$(dirname "$0")/kumaboard.service" /etc/systemd/system/kumaboard.service
systemctl daemon-reload
systemctl enable kumaboard
echo "Copy the kumaboard binary to /opt/kumaboard/ and config to /etc/kumaboard/config.yaml, then: systemctl start kumaboard"
echo "Then: sudo -u kumaboard /opt/kumaboard/kumaboard passwd -config /etc/kumaboard/config.yaml"
echo "The release signing key (pki/release_ed25519) must stay root:root 0600; the kumaboard user must not read it."
echo "Install the nightly backup with deploy/backup/install.sh (see deploy/backup/restore.md)."
