#!/usr/bin/env bash
# bunk agent installer for a shared (host) machine.
# Linux, including WSL2 Ubuntu on Windows.
#
# Usage:  curl -fsSL https://raw.githubusercontent.com/root-explorer/bunk/main/scripts/install-agent.sh \
#           | sudo bash -s -- <hub-url> <hub-token> [machine-name]
#
# Prints a pairing code at the end — send it to the person you're sharing with.
set -euo pipefail

HUB="${1:-}"
TOKEN="${2:-}"
NAME="${3:-$(hostname)}"

if [ -z "$HUB" ] || [ -z "$TOKEN" ]; then
  echo "usage: install-agent.sh <hub-url> <hub-token> [machine-name]" >&2
  exit 1
fi

# 1. Download the static binary for this architecture.
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  ARCH=amd64 ;;
  aarch64) ARCH=arm64 ;;
  arm64)   ARCH=arm64 ;;
  *) echo "unsupported architecture: $ARCH" >&2; exit 1 ;;
esac
URL="https://github.com/root-explorer/bunk/releases/latest/download/bunk-linux-$ARCH"
echo "==> downloading $URL"
curl -fL -o /usr/local/bin/bunk "$URL"
chmod 0755 /usr/local/bin/bunk

# 2. Config: safe defaults (6 cpu / 12 GB / 256 pids).
#    Tune these to what you're comfortable lending — the flags are injected
#    only when the guest doesn't specify their own.
mkdir -p /root/.bunk
cat > /root/.bunk/config.yaml <<EOF
hub: $HUB
limits: {cpus: 6, memory_gb: 12, pids: 256}
EOF

# 3. Hub token for the agent service.
mkdir -p /etc/bunk-agent
echo "BUNK_HUB_TOKEN=$TOKEN" > /etc/bunk-agent/env

# 4. systemd unit (survives reboots; WSL2 Ubuntu runs systemd by default).
cat > /etc/systemd/system/bunk-agent.service <<'EOF'
[Unit]
Description=bunk agent (remote machine sharing)
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
User=root
EnvironmentFile=/etc/bunk-agent/env
Environment=BUNK_HOME=/root/.bunk
ExecStart=/usr/local/bin/bunk daemon
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable bunk-agent
systemctl restart bunk-agent

# 5. Print the pairing code.
sleep 2
echo
BUNK_HOME=/root/.bunk BUNK_HUB_TOKEN="$TOKEN" /usr/local/bin/bunk pair --name "$NAME"
