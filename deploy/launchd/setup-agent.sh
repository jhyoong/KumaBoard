#!/usr/bin/env bash
set -euo pipefail

if ! id kuma-agent >/dev/null 2>&1; then
  PW=$(openssl rand -base64 24)
  sysadminctl -addUser kuma-agent -fullName "KumaBoard Agent" -password "$PW" -home /var/kuma-agent -shell /bin/bash
  dscl . create /Users/kuma-agent IsHidden 1
  install -d -o kuma-agent -g staff -m 0750 /var/kuma-agent
  echo "Created user kuma-agent (standard, hidden). Password stored nowhere; use su - from an admin session."
fi

install -d -m 0755 -o kuma-agent -g staff /opt/kuma-agent
install -d -m 0755 -o root -g wheel /etc/kuma-agent
# Scripts run from the dashboard (script: in config.yaml) live here. Root
# owns the directory and every script in it; the agent can read and execute
# them, never change them. The agent checks this itself and withholds the
# button for any script, or directory on the way to it, that its own account
# could modify.
install -d -m 0755 -o root -g wheel /opt/kuma-scripts
touch /var/log/kuma-agent.log && chown kuma-agent:staff /var/log/kuma-agent.log
install -m 0644 -o root -g wheel "$(dirname "$0")/com.kumaboard.agent.plist" /Library/LaunchDaemons/com.kumaboard.agent.plist

cat <<EOF
Next:
  cp kuma-agent  /opt/kuma-agent/kuma-agent && chown kuma-agent:staff /opt/kuma-agent/kuma-agent && chmod 0755 /opt/kuma-agent/kuma-agent
  cp config.yaml /etc/kuma-agent/config.yaml   (root-owned, 0644)
  cp ca.pem      /etc/kuma-agent/ca.pem        (root-owned, 0644)
  install -m 0600 -o kuma-agent -g staff token /etc/kuma-agent/token
  install -m 0755 -o root -g wheel myscript.sh /opt/kuma-scripts/myscript.sh   (optional: one per script: entry)
  launchctl bootstrap system /Library/LaunchDaemons/com.kumaboard.agent.plist
  tail -f /var/log/kuma-agent.log
Record whether FileVault is on (fdesetup status): if so, a reboot needs a hands-on unlock.
EOF
