#!/usr/bin/env bash
# Nightly KumaBoard backup. Runs as root from the systemd timer on the control plane host.
# Requires: sqlite3, age, an SSH key for the backup host at /root/.ssh/kumaboard_backup.
set -euo pipefail

DATA=/var/lib/kumaboard
CONF=/etc/kumaboard
RECIPIENT_FILE=$CONF/backup.age.pub      # age public key; private key lives only in the password manager
REMOTE=${KUMABOARD_BACKUP_REMOTE:-admin@192.168.1.31}   # backup host (any always-on machine with SSH)
REMOTE_DIR=backups/kumaboard
KEEP=7
STAMP=$(date +%Y%m%d-%H%M%S)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$DATA/backup"
sqlite3 "$DATA/kumaboard.db" ".backup '$WORK/kumaboard.db'"
tar -C / -cf "$WORK/kumaboard-$STAMP.tar" \
  -C "$WORK" kumaboard.db \
  -C / var/lib/kumaboard/pki var/lib/kumaboard/releases etc/kumaboard
age -r "$(cat "$RECIPIENT_FILE")" -o "$DATA/backup/kumaboard-$STAMP.tar.age" "$WORK/kumaboard-$STAMP.tar"

scp -i /root/.ssh/kumaboard_backup -o StrictHostKeyChecking=accept-new \
  "$DATA/backup/kumaboard-$STAMP.tar.age" "$REMOTE:$REMOTE_DIR/"

# Retention on both sides.
ls -1t "$DATA/backup"/kumaboard-*.tar.age | tail -n +$((KEEP + 1)) | xargs -r rm -f
ssh -i /root/.ssh/kumaboard_backup "$REMOTE" "ls -1t $REMOTE_DIR/kumaboard-*.tar.age | tail -n +$((KEEP + 1)) | xargs -r rm -f"

echo "kumaboard backup ok: kumaboard-$STAMP.tar.age"
