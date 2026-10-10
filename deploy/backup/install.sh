#!/usr/bin/env bash
# Installs the nightly backup on the control plane host. Run as root, after
# setup-server.sh. Safe to re-run; it never overwrites /etc/kumaboard/backup.env.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
env_file=/etc/kumaboard/backup.env

[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }
for tool in sqlite3 age; do
  command -v "$tool" >/dev/null || { echo "$tool is not installed (apt install $tool)" >&2; exit 1; }
done

install -d -m 0755 -o root -g root /opt/kumaboard /etc/kumaboard
install -m 0755 -o root -g root "$here/backup.sh" /opt/kumaboard/backup.sh
[ -e "$env_file" ] || install -m 0600 -o root -g root "$here/backup.env.example" "$env_file"
for unit in kumaboard-backup.service kumaboard-backup-failed.service kumaboard-backup.timer; do
  install -m 0644 -o root -g root "$here/$unit" "/etc/systemd/system/$unit"
done
systemctl daemon-reload

# The timer is enabled only once a run can succeed, so a half-configured host
# does not fail every night.
ready=1
# shellcheck disable=SC1090
setting() { (set -a; . "$env_file"; eval "printf %s \"\${$1:-$2}\""); }
remote=$(setting KUMABOARD_BACKUP_REMOTE "")
recipient=$(setting KUMABOARD_BACKUP_RECIPIENT_FILE /etc/kumaboard/backup.age.pub)
ssh_key=$(setting KUMABOARD_BACKUP_SSH_KEY /root/.ssh/kumaboard_backup)
[ -n "$remote" ] || { echo "todo: set KUMABOARD_BACKUP_REMOTE in $env_file"; ready=0; }
[ -r "$recipient" ] || { echo "todo: put the age public key in $recipient (age-keygen; keep the private key off this host)"; ready=0; }
[ -r "$ssh_key" ] || { echo "todo: create $ssh_key and authorise it on the backup host (ssh-keygen -t ed25519 -N '' -f $ssh_key)"; ready=0; }

if [ "$ready" -eq 1 ]; then
  systemctl enable --now kumaboard-backup.timer
  echo "backup timer enabled. Run one now and check it: systemctl start kumaboard-backup.service"
else
  echo "finish the steps above, then re-run this script."
fi
