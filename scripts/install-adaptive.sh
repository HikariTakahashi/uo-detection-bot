#!/bin/bash
# Explicit root handoff. Never reads or rewrites the Discord token/config/counts.
set -euo pipefail
mode=${1:-prepare}
source_dir=$(cd -- "$(dirname -- "$0")/.." && pwd)
[[ $(id -u) == 0 ]] || { echo 'Run this reviewed installer with sudo.' >&2; exit 1; }
image=sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b
case "$mode" in
prepare)
    [[ $(uname -m) == aarch64 ]] || exit 1
    apt-get update
    apt-get install -y docker.io gh git
    systemctl enable --now docker.service
    docker pull "python@$image"
    [[ $(docker image inspect "python@$image" --format '{{.Architecture}}') == arm64 ]] || exit 1
    id laya-decoder >/dev/null 2>&1 || useradd --system --create-home --home-dir /var/lib/laya-decoder --shell /usr/sbin/nologin laya-decoder
    usermod -a -G docker laya-decoder
    install -d -o root -g root -m 0755 /etc/laya-decoder/public-templates /opt/laya-decoder/releases
    install -d -o laya-decoder -g laya-decoder -m 0700 /var/lib/laya-decoder
    echo 'Prerequisites ready. No Bot settings or services changed.'
    ;;
credentials)
    install -d -o root -g root -m 0755 /etc/laya-decoder
    umask 077
    for item in github-token luna-key; do
        if [[ -s /etc/laya-decoder/$item ]]; then echo "$item already configured; keeping it."; continue; fi
        read -r -s -p "Enter Pi-specific $item (hidden; empty skips): " decoder_secret </dev/tty
        echo >/dev/tty
        if [[ -n $decoder_secret ]]; then printf '%s' "$decoder_secret" > "/etc/laya-decoder/$item"; chmod 0600 "/etc/laya-decoder/$item"; fi
        unset decoder_secret
    done
    ;;
install)
    # The source archive contains only reviewed runtime files and an ARM64 Bot.
    test -f "$source_dir/DEPLOY-SHA256SUMS"
    (cd "$source_dir" && sha256sum --check --quiet DEPLOY-SHA256SUMS)
    test -x "$source_dir/discord-bot-linux-arm64"
    version=$("$source_dir/discord-bot-linux-arm64" -version)
    [[ $version =~ ^[a-f0-9]{40}$ ]] || { echo 'Require a reviewed commit build.' >&2; exit 1; }
    target="/opt/laya-decoder/releases/$version"
    if [[ -e $target ]]; then echo 'Release directory already exists; verify instead of overwriting.' >&2; exit 1; fi
    install -d -m 0755 "$target"
    cp -a "$source_dir/services" "$source_dir/experiments" "$target/"
    chown -R root:root "$target"
    chmod -R go-w "$target"
    ln -s "$target" /opt/laya-decoder/next
    mv -Tf /opt/laya-decoder/next /opt/laya-decoder/current
    for unit in laya-decoder.service laya-decoder-update.service laya-decoder-update.timer laya-decoder-generate.service laya-decoder-generate.timer laya-decoder-approval.service laya-decoder-approval.timer laya-decoder-publication.service laya-decoder-publication.timer; do
        install -m 0644 "$source_dir/deploy/$unit" "/etc/systemd/system/$unit"
    done
    # Backup every replaced inference adapter file. Reuse existing offline model.
    backup="/var/backups/laya-adaptive-$version"
    install -d -m 0700 "$backup"
    for name in serve_shadow.py production_policy.py; do
        [[ ! -e /home/user/laya-sandbox/$name ]] || cp -a "/home/user/laya-sandbox/$name" "$backup/"
        install -o user -g user -m 0644 "$source_dir/experiments/laya/$name" "/home/user/laya-sandbox/$name"
    done
    install -o root -g root -m 0644 "$source_dir/services/decoder/initial-release.json" /etc/laya-decoder/approved-release.json
    systemctl daemon-reload
    uid=$(id -u user)
    runuser -u user -- env XDG_RUNTIME_DIR="/run/user/$uid" systemctl --user restart laya-shadow.service
    systemctl start laya-decoder-update.service
    systemctl enable --now laya-decoder.service laya-decoder-update.timer laya-decoder-generate.timer
    echo 'Decoder services installed. Production count connection is still unchanged; run verification before enable.'
    ;;
approvals)
    # Enable only after the release containing the DM handler is installed.
    (cd "$source_dir" && sha256sum --check --quiet DEPLOY-SHA256SUMS)
    version=$("$source_dir/discord-bot-linux-arm64" -version)
    [[ $(/opt/discord-bot/current/discord-bot -version) == "$version" ]] || exit 1
    test -f /var/lib/laya-decoder/current/active-production.json
    test -f /opt/laya-decoder/current/services/decoder/approval.py
    # Fail before switching modes if the private evaluation CI is not readable.
    systemd-run --quiet --wait --pipe --collect \
        -p User=laya-decoder \
        -p LoadCredential=github-token:/etc/laya-decoder/github-token \
        -p Environment=PYTHONDONTWRITEBYTECODE=1 \
        /usr/bin/python3 -c 'import sys; sys.path.insert(0,"/opt/laya-decoder/current/services/decoder"); import approval,release; release.credential("github-token","GH_TOKEN"); approval.ci_success(approval.current("/var/lib/laya-decoder"))'
    getent group laya-approvals >/dev/null || groupadd --system laya-approvals
    usermod -a -G laya-approvals discord-bot
    usermod -a -G laya-approvals laya-decoder
    install -d -o root -g laya-approvals -m 0750 /var/lib/laya-approvals
    for name in requests results; do
        install -d -o laya-decoder -g laya-approvals -m 2750 "/var/lib/laya-approvals/$name"
    done
    install -d -o discord-bot -g laya-approvals -m 2750 /var/lib/laya-approvals/decisions
    install -d -m 0755 /etc/systemd/system/discord-bot.service.d
    cat > /etc/systemd/system/discord-bot.service.d/approval.conf <<'EOF'
[Service]
SupplementaryGroups=laya-approvals
ReadWritePaths=/var/lib/laya-approvals/decisions
EOF
    # The root pin remains untouched for audit; DM mode tracks the live snapshot.
    systemctl disable --now laya-decoder-update.timer
    systemctl stop laya-decoder-update.service
    printf '%s\n' '{"enabled":true,"approver":"845282954636951583","repository":"HikariTakahashi/uo-decoder-evaluation"}' > /etc/laya-decoder/dm-approval.json
    chmod 0644 /etc/laya-decoder/dm-approval.json
    systemctl daemon-reload
    systemctl restart discord-bot.service
    systemctl enable --now laya-decoder-approval.timer
    echo 'Production adoption DM approvals enabled; current decoder snapshot retained.'
    ;;
automatic)
    # Requires separate explicit consent to this bounded future publication policy.
    (cd "$source_dir" && sha256sum --check --quiet DEPLOY-SHA256SUMS)
    test -f /etc/laya-decoder/dm-approval.json
    test -f /opt/laya-decoder/current/services/decoder/autopublish.py
    printf '%s\n' '{"schema":1,"synthetic_recipes":["x9"],"publish_validated":true,"repository":"HikariTakahashi/uo-decoder-evaluation","daily_jobs":3}' > /etc/laya-decoder/automatic.json
    chown root:root /etc/laya-decoder/automatic.json
    chmod 0644 /etc/laya-decoder/automatic.json
    systemctl daemon-reload
    systemctl enable --now laya-decoder-generate.timer laya-decoder-publication.timer laya-decoder-approval.timer
    echo 'Automatic x9 synthetic preparation and validated data publication enabled; production adoption still requires DM approval.'
    ;;
enable)
    test -s /etc/laya-decoder/github-token
    test -s /etc/laya-decoder/luna-key
    (cd "$source_dir" && sha256sum --check --quiet DEPLOY-SHA256SUMS)
    python3 - <<'PY'
import json,urllib.request
with urllib.request.urlopen('http://127.0.0.1:8767/health',timeout=5) as r:
    result=json.load(r)
assert result['status']=='ready' and len(result['release'])==64
PY
    version=$("$source_dir/discord-bot-linux-arm64" -version)
    [[ $version =~ ^[a-f0-9]{40}$ ]] || exit 1
    backup="/var/backups/discord-adaptive-$version"
    install -d -m 0700 "$backup"
    cp -a /opt/discord-bot/current "$backup/current"
    [[ ! -e /etc/discord-bot/adaptive.json ]] || cp -a /etc/discord-bot/adaptive.json "$backup/adaptive.json"
    # Keep counts and receipt format migration recoverable; copy under stopped writer.
    systemctl stop discord-bot-update.timer
    systemctl stop discord-bot-update.service || true
    systemctl stop discord-bot.service
    cp -a /var/lib/discord-bot/counts.json "$backup/counts-before.json"
    install -d -m 0755 "/opt/discord-bot/releases/$version"
    install -m 0755 "$source_dir/discord-bot-linux-arm64" "/opt/discord-bot/releases/$version/discord-bot"
    ln -s "releases/$version" /opt/discord-bot/adaptive-next
    mv -Tf /opt/discord-bot/adaptive-next /opt/discord-bot/current
    printf '%s\n' '{"enabled":true}' > /etc/discord-bot/adaptive.json
    chown root:discord-bot /etc/discord-bot/adaptive.json
    chmod 0640 /etc/discord-bot/adaptive.json
    if ! systemctl start discord-bot.service; then
        # A started process may already have persisted v2 receipts. Keep the new
        # compatible binary, disable adaptive intake, and preserve current counts.
        printf '%s\n' '{"enabled":false}' > /etc/discord-bot/adaptive.json
        systemctl restart discord-bot.service || true
        echo 'Enable failed; adaptive disabled and counts retained. Inspect journal.' >&2
        exit 1
    fi
    systemctl is-active --quiet discord-bot.service
    echo 'Production review/count connection enabled. Bot updater remains stopped until this build is published on main.'
    ;;
disable)
    printf '%s\n' '{"enabled":false}' > /etc/discord-bot/adaptive.json
    chown root:discord-bot /etc/discord-bot/adaptive.json;chmod 0640 /etc/discord-bot/adaptive.json
    systemctl restart discord-bot.service
    echo 'Adaptive intake/counting stopped; receipts/counts retained. Do not restore an old binary over v2 counts.'
    ;;
*) echo 'Usage: install-adaptive.sh prepare|credentials|install|approvals|automatic|enable|disable' >&2;exit 2;;
esac
