#!/usr/bin/env bash
set -Eeuo pipefail

# Install this script on m0n0arm. It pulls the bacli release directory from
# iaia-box using the already trusted SSH key, validates it, and publishes it.
REMOTE_HOST=${BACLI_REMOTE_HOST:-ubuntu@iaia-box}
REMOTE_DIR=${BACLI_REMOTE_DIR:-/home/ubuntu/CODEX/build-agent-go-cli/dist/}
WEB_DIR=${BACLI_WEB_DIR:-/var/www/html/bacli}
STATE_DIR=${BACLI_MIRROR_STATE_DIR:-/var/lib/bacli-mirror}
LOCK_FILE=${BACLI_MIRROR_LOCK_FILE:-/run/lock/bacli-mirror.lock}
SSH_OPTS=${BACLI_SSH_OPTS:--o BatchMode=yes -o ConnectTimeout=15}

log() {
  printf '%s bacli-mirror: %s\n' "$(date -Is)" "$*"
}

fail() {
  log "ERROR: $*" >&2
  exit 1
}

for command in ssh rsync flock python3 sha256sum find cmp mktemp; do
  command -v "$command" >/dev/null 2>&1 || fail "$command is required"
done

if [[ ${EUID:-$(id -u)} -ne 0 && ${BACLI_ALLOW_NON_ROOT:-0} != 1 ]]; then
  fail "run as root so /var/www/html/bacli can be replaced safely"
fi

mkdir -p "$(dirname "$LOCK_FILE")" "$STATE_DIR" "$(dirname "$WEB_DIR")"
exec 9>"$LOCK_FILE"
if ! flock -n 9; then
  log "another mirror run is active; skipping"
  exit 0
fi

cache="$STATE_DIR/current"
mkdir -p "$cache"
incoming=$(mktemp -d "$STATE_DIR/incoming.XXXXXX")
publish=$(mktemp -d "$(dirname "$WEB_DIR")/.bacli-publish.XXXXXX")
cleanup() {
  rm -rf "$incoming" "$publish"
}
trap cleanup EXIT HUP INT TERM

# Seed from the persistent cache so rsync transfers only changed remote files.
rsync -a --delete "$cache/" "$incoming/"
# shellcheck disable=SC2086
rsync -a --delete --delay-updates --partial \
  -e "ssh $SSH_OPTS" \
  "$REMOTE_HOST:$REMOTE_DIR" "$incoming/"

[[ -f "$incoming/version.json" ]] || fail "remote dist has no version.json"
[[ -f "$incoming/SHA256SUMS" ]] || fail "remote dist has no SHA256SUMS"
[[ -f "$incoming/install.sh" ]] || fail "remote dist has no install.sh"
[[ -f "$incoming/install.ps1" ]] || fail "remote dist has no install.ps1"

python3 - "$incoming" <<'PY'
import json, pathlib, re, sys
root = pathlib.Path(sys.argv[1])
manifest = json.loads((root / 'version.json').read_text(encoding='utf-8'))
if manifest.get('schemaVersion') != 1 or not manifest.get('version'):
    raise SystemExit('invalid version.json')
files = manifest.get('files')
if not isinstance(files, dict) or not files:
    raise SystemExit('version.json contains no release files')
for target, item in files.items():
    if not isinstance(item, dict):
        raise SystemExit(f'invalid manifest entry for {target}')
    name = item.get('url', '')
    digest = item.get('sha256', '')
    if '/' in name or '\\' in name or name in ('', '.', '..'):
        raise SystemExit(f'unsafe artifact name for {target}: {name!r}')
    if not re.fullmatch(r'[0-9a-fA-F]{64}', digest):
        raise SystemExit(f'invalid checksum for {target}')
    if not (root / name).is_file():
        raise SystemExit(f'missing artifact for {target}: {name}')
PY

(
  cd "$incoming"
  sha256sum -c SHA256SUMS
) >/dev/null

# Ignore directory mtimes when deciding whether publication content changed.
if [[ -d "$cache" ]] && diff -qr --no-dereference "$cache" "$incoming" >/dev/null 2>&1; then
  log "no release changes"
  exit 0
fi

# Refresh the persistent cache only after validation succeeds.
rsync -a --delete "$incoming/" "$cache/"

# Build the public tree separately. version.json is copied last so clients never
# see a manifest that references artifacts which have not been published yet.
rsync -a --delete --exclude=/version.json "$cache/" "$publish/"
cp -p "$cache/version.json" "$publish/version.json"

# Replace the web tree with --delete semantics. Files are staged first and the
# manifest is still transferred last inside the final tree update.
mkdir -p "$WEB_DIR"
rsync -a --delete --delay-updates --exclude=/version.json "$publish/" "$WEB_DIR/"
cp -p "$publish/version.json" "$WEB_DIR/version.json"
find "$WEB_DIR" -type d -exec chmod 0755 {} +
find "$WEB_DIR" -type f -exec chmod 0644 {} +

log "published bacli release $(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["version"])' "$WEB_DIR/version.json")"
