# Example per-device deploy targets. Copy to deploy/hosts.local.mk (gitignored)
# and replace the SSH users and addresses with your own.
#
# Format: DEPLOY_<device> := <ssh target> <GOOS> <GOARCH> '<restart command>'
# Then: make deploy-<device>

# Linux (systemd)
DEPLOY_linux-box := admin@192.168.1.20 linux amd64 'sudo install -o kuma-agent -g kuma-agent -m 0755 /tmp/kuma-agent.new /opt/kuma-agent/kuma-agent && sudo systemctl restart kuma-agent'

# Lightweight Linux devices (Raspberry Pi and similar, arm64)
DEPLOY_pi := admin@192.168.1.21 linux arm64 'sudo install -o kuma-agent -g kuma-agent -m 0755 /tmp/kuma-agent.new /opt/kuma-agent/kuma-agent && sudo systemctl restart kuma-agent'

# macOS (launchd)
DEPLOY_macos-workstation := admin@192.168.1.30 darwin arm64 'sudo install -o kuma-agent -g staff -m 0755 /tmp/kuma-agent.new /opt/kuma-agent/kuma-agent && sudo launchctl kickstart -k system/com.kumaboard.agent'

# Windows (SCM). Needs the OpenSSH server feature; scp lands in the user's home.
DEPLOY_windows-desktop := admin@192.168.1.40 windows amd64 'sc stop kuma-agent & timeout /t 3 & move /Y C:\Users\admin\kuma-agent.new C:\ProgramData\kuma-agent\bin\kuma-agent.exe & sc start kuma-agent'
