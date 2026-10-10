#!/usr/bin/env bash
# Nightly KumaBoard backup. Runs as root from the systemd timer on the control
# plane host (root, because the release signing key is readable by root only).
#
# Settings come from the environment; the timer's service reads them from
# /etc/kumaboard/backup.env (see backup.env.example). Requires sqlite3, age,
# and an SSH key that can reach the backup host.
set -euo pipefail

DATA=${KUMABOARD_DATA_DIR:-/var/lib/kumaboard}
CONFIG=${KUMABOARD_CONFIG:-/etc/kumaboard/config.yaml}
# age public key; the private key lives only in the password manager.
RECIPIENT_FILE=${KUMABOARD_BACKUP_RECIPIENT_FILE:-/etc/kumaboard/backup.age.pub}
# Backup host, as user@host: any always-on machine with SSH. No default, so a
# real address never has to live in the repo.
REMOTE=${KUMABOARD_BACKUP_REMOTE:?set KUMABOARD_BACKUP_REMOTE to user@host of the backup host}
REMOTE_DIR=${KUMABOARD_BACKUP_REMOTE_DIR:-backups/kumaboard}
SSH_KEY=${KUMABOARD_BACKUP_SSH_KEY:-/root/.ssh/kumaboard_backup}
LOCAL_DIR=${KUMABOARD_BACKUP_DIR:-$DATA/backup}
KEEP=${KUMABOARD_BACKUP_KEEP:-7}

for tool in sqlite3 age tar scp ssh; do
  command -v "$tool" >/dev/null || { echo "kumaboard backup: $tool is not installed" >&2; exit 1; }
done
for f in "$DATA/kumaboard.db" "$DATA/pki/ca.key" "$CONFIG" "$RECIPIENT_FILE" "$SSH_KEY"; do
  [ -r "$f" ] || { echo "kumaboard backup: cannot read $f" >&2; exit 1; }
done
case $KEEP in ''|*[!0-9]*|0) echo "kumaboard backup: KUMABOARD_BACKUP_KEEP must be a positive number" >&2; exit 1 ;; esac

umask 077
STAMP=$(date +%Y%m%d-%H%M%S)
NAME=kumaboard-$STAMP.tar.age
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# The archive holds paths relative to its root (kumaboard.db, config.yaml,
# pki/, releases/), so it restores into any data directory.
sqlite3 "$DATA/kumaboard.db" ".backup '$WORK/kumaboard.db'"
cp "$CONFIG" "$WORK/config.yaml"
members=(-C "$WORK" kumaboard.db config.yaml -C "$DATA" pki)
[ -d "$DATA/releases" ] && members+=(releases)
tar -cf "$WORK/kumaboard.tar" "${members[@]}"

mkdir -p "$LOCAL_DIR"
# Written under a temporary name so retention never counts a partial archive.
age -r "$(cat "$RECIPIENT_FILE")" -o "$LOCAL_DIR/.$NAME.partial" "$WORK/kumaboard.tar"
mv "$LOCAL_DIR/.$NAME.partial" "$LOCAL_DIR/$NAME"

ssh_opts=(-i "$SSH_KEY" -o BatchMode=yes -o StrictHostKeyChecking=accept-new)
ssh "${ssh_opts[@]}" "$REMOTE" "mkdir -p '$REMOTE_DIR'"
scp "${ssh_opts[@]}" "$LOCAL_DIR/$NAME" "$REMOTE:$REMOTE_DIR/"

# Retention on both sides.
ls -1t "$LOCAL_DIR"/kumaboard-*.tar.age | tail -n +$((KEEP + 1)) | xargs -r rm -f
ssh "${ssh_opts[@]}" "$REMOTE" "ls -1t '$REMOTE_DIR'/kumaboard-*.tar.age | tail -n +$((KEEP + 1)) | xargs -r rm -f"

echo "kumaboard backup ok: $NAME"
