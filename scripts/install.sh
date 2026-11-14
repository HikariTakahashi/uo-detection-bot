#!/usr/bin/env bash
set -euo pipefail
if [[ $(id -u) != 0 ]]; then
  echo 'Run with sudo: sudo bash scripts/install.sh' >&2
  exit 1
fi
if [[ $(uname -m) != aarch64 ]]; then
  echo 'This deployment targets Linux ARM64.' >&2
  exit 1
fi
repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
for file in deploy/discord-bot.service deploy/discord-bot-update.service deploy/discord-bot-update.timer scripts/update-bot.py detection.config.example.json; do
  test -f "$repo_dir/$file"
done
if [[ ! -f /etc/discord-bot.env ]]; then
  install -o root -g root -m 0600 "$repo_dir/.env.example" /etc/discord-bot.env
  echo 'Set DISCORD_TOKEN and optional DISCORD_GUILD_ID in /etc/discord-bot.env, then rerun.' >&2
  exit 1
fi
if ! grep -Eq '^DISCORD_TOKEN=.+$' /etc/discord-bot.env; then
  echo 'DISCORD_TOKEN is missing from /etc/discord-bot.env.' >&2
  exit 1
fi
backup_dir=$(mktemp -d /var/backups/discord-bot.XXXXXXXX)
chmod 0700 "$backup_dir"
for file in /etc/systemd/system/discord-bot.service /etc/systemd/system/discord-bot-update.service /etc/systemd/system/discord-bot-update.timer /etc/discord-bot.env /etc/discord-bot/config.json /opt/discord-bot/discord-bot; do
  if [[ -f "$file" ]]; then cp --parents -p "$file" "$backup_dir/"; fi
done
if ! getent group discord-bot >/dev/null; then groupadd --system discord-bot; fi
if ! id discord-bot >/dev/null 2>&1; then
  useradd --system --gid discord-bot --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin discord-bot
fi
# Stop any old service before changing its executable path and service type.
systemctl stop discord-bot.service
install -d -o root -g root -m 0755 /opt/discord-bot /opt/discord-bot/releases /usr/local/lib/discord-bot
install -d -o root -g root -m 0700 /var/lib/discord-bot-update
install -d -o root -g discord-bot -m 0750 /etc/discord-bot
if [[ ! -e /etc/discord-bot/config.json ]]; then
  install -o root -g discord-bot -m 0640 "$repo_dir/detection.config.example.json" /etc/discord-bot/config.json
fi
chown root:root /etc/discord-bot.env
chmod 0600 /etc/discord-bot.env
install -o root -g root -m 0644 "$repo_dir/scripts/update-bot.py" /usr/local/lib/discord-bot/update-bot.py
for unit in discord-bot.service discord-bot-update.service discord-bot-update.timer; do
  install -o root -g root -m 0644 "$repo_dir/deploy/$unit" "/etc/systemd/system/$unit"
done
systemctl daemon-reload
systemctl reset-failed discord-bot.service
systemctl enable discord-bot.service
systemctl enable --now discord-bot-update.timer
systemctl start discord-bot-update.service
printf 'Installed. Backup: %s\n' "$backup_dir"
printf 'Check: systemctl status discord-bot.service discord-bot-update.timer\n'
