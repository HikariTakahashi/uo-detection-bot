#!/usr/bin/python3
"""Pull a completed release for the exact main commit, then switch atomically."""
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.request

REPO = "HikariTakahashi/uo-detection-bot"
API = "https://api.github.com/repos/" + REPO
ROOT = Path("/opt/discord-bot")
STATE = Path("/var/lib/discord-bot-update")
UNIT = "discord-bot.service"
ASSET = "discord-bot-linux-arm64"
SHA_RE = re.compile(r"[0-9a-f]{40}")
STABILITY_SECONDS = 10


def log(message):
    print(message, flush=True)


def request(url, limit=2 * 1024 * 1024):
    req = urllib.request.Request(url, headers={
        "User-Agent": "uo-detection-bot-updater/1.0",
        "Accept": "application/vnd.github+json" if url.startswith(API) else "application/octet-stream",
    })
    with urllib.request.urlopen(req, timeout=30) as response:
        data = response.read(limit + 1)
        if len(data) > limit:
            raise ValueError("download exceeds size limit")
        return data


def get_json(url):
    return json.loads(request(url))


def write_state(value):
    temporary = STATE / "state.tmp"
    with temporary.open("w") as f:
        json.dump(value, f)
        f.flush()
        os.fsync(f.fileno())
    os.replace(temporary, STATE / "state.json")
    fd = os.open(STATE, os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def read_state():
    p = STATE / "state.json"
    return json.loads(p.read_text()) if p.exists() else {}


def current_sha():
    link = ROOT / "current"
    if not link.is_symlink():
        if link.exists():
            raise RuntimeError("current must be a managed symlink")
        return None
    target = os.readlink(link)
    if not target.startswith("releases/") or not SHA_RE.fullmatch(target[9:]):
        raise RuntimeError("unexpected current symlink target")
    return target[9:]


def switch(sha):
    if not SHA_RE.fullmatch(sha):
        raise ValueError("invalid commit SHA")
    temporary = ROOT / "current.new"
    temporary.unlink(missing_ok=True)
    temporary.symlink_to("releases/" + sha)
    os.replace(temporary, ROOT / "current")
    fd = os.open(ROOT, os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def systemctl(*args):
    return subprocess.run(["/usr/bin/systemctl", *args, UNIT], check=True,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=110)


def rollback(previous):
    if previous:
        switch(previous)
        systemctl("reset-failed")
        systemctl("restart")
        systemctl("is-active", "--quiet")
    else:
        systemctl("stop")
        (ROOT / "current").unlink(missing_ok=True)


def recover(state):
    pending = state.get("pending")
    if pending:
        log("Recovering interrupted update " + pending["sha"])
        rollback(pending["previous"])
        state["failed"] = pending["sha"]
        state["current"] = pending["previous"]
        del state["pending"]
        write_state(state)


def prepare(sha, release):
    if release.get("draft") or release.get("tag_name") != "deploy-" + sha:
        raise ValueError("release not ready or wrong tag")
    names = {asset["name"] for asset in release.get("assets", [])}
    if not {ASSET, "SHA256SUMS"} <= names:
        raise ValueError("release is missing required assets")
    base = "https://github.com/" + REPO + "/releases/download/deploy-" + sha + "/"
    checksums = request(base + "SHA256SUMS", 4096).decode("ascii")
    match = re.fullmatch(r"([0-9a-f]{64})  " + re.escape(ASSET) + r"\n?", checksums)
    if not match:
        raise ValueError("invalid checksum manifest")
    binary = request(base + ASSET, 64 * 1024 * 1024)
    if hashlib.sha256(binary).hexdigest() != match.group(1):
        raise ValueError("binary checksum mismatch")
    # ELF64, little endian, AArch64. Never execute downloaded code as root.
    if binary[:6] != b"\x7fELF\x02\x01" or binary[18:20] != b"\xb7\x00":
        raise ValueError("binary is not Linux ARM64")
    directory = ROOT / "releases" / sha
    directory.mkdir(parents=True, exist_ok=True)
    if directory.is_symlink():
        raise ValueError("release directory cannot be a symlink")
    # The updater uses UMask=0077; the unprivileged bot must traverse this directory.
    directory.chmod(0o755)
    temporary = directory / "discord-bot.new"
    with temporary.open("wb") as f:
        f.write(binary)
        f.flush()
        os.fsync(f.fileno())
    temporary.chmod(0o755)
    os.replace(temporary, directory / "discord-bot")


def deploy(sha, state):
    previous = current_sha()
    state["pending"] = {"sha": sha, "previous": previous}
    write_state(state)
    try:
        switch(sha)
        systemctl("reset-failed")
        # Type=notify: restart returns only after Discord READY and command registration.
        systemctl("restart")
        pid = systemctl("show", "--property=MainPID", "--value").stdout.strip()
        if not pid or pid == b"0":
            raise RuntimeError("no running bot process")
        for _ in range(STABILITY_SECONDS):
            time.sleep(1)
            systemctl("is-active", "--quiet")
            if systemctl("show", "--property=MainPID", "--value").stdout.strip() != pid:
                raise RuntimeError("bot restarted during readiness check")
    except BaseException:
        log("New bot failed readiness; restoring previous version")
        rollback(previous)
        state["failed"] = sha
        state["current"] = previous
        del state["pending"]
        write_state(state)
        raise
    state["current"] = sha
    state.pop("pending", None)
    state.pop("failed", None)
    write_state(state)
    log("Deployed " + sha)


def update():
    state = read_state()
    recover(state)
    sha = get_json(API + "/git/ref/heads/main")["object"]["sha"]
    if not SHA_RE.fullmatch(sha):
        raise ValueError("invalid main SHA")
    if sha == current_sha():
        log("Already deployed " + sha)
        return
    if sha == state.get("failed"):
        log("Skipping previously failed commit " + sha)
        return
    try:
        release = get_json(API + "/releases/tags/deploy-" + sha)
    except urllib.error.HTTPError as error:
        if error.code == 404:
            log("Waiting for successful CI release " + sha)
            return
        raise
    prepare(sha, release)
    # A newer commit may have landed while downloading. Never deploy a stale head.
    if get_json(API + "/git/ref/heads/main")["object"]["sha"] != sha:
        log("main changed while downloading; retry on next timer")
        return
    deploy(sha, state)


def main():
    STATE.mkdir(parents=True, exist_ok=True)
    with (STATE / "update.lock").open("a") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            log("Another updater is running")
            return
        update()


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))
    try:
        main()
    except Exception as error:
        # Do not dump HTTP bodies, environment or credentials into journals.
        log("Update failed: " + type(error).__name__ + ": " + str(error))
        sys.exit(1)
