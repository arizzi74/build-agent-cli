#!/bin/sh
set -eu

BASE_URL=${BACLI_BASE_URL:-https://nowdemo.it/bacli}
INSTALL_DIR=${BACLI_INSTALL_DIR:-"$HOME/.local/bin"}
MANIFEST_URL="$BASE_URL/version.json"

fail() {
  printf 'bacli installer: %s\n' "$*" >&2
  exit 1
}

command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v python3 >/dev/null 2>&1 || fail "python3 is required to read the signed-by-checksum release manifest"

os=$(uname -s)
arch=$(uname -m)
case "$os/$arch" in
  Linux/aarch64|Linux/arm64) target=linux-arm64 ;;
  Darwin/x86_64|Darwin/amd64) target=darwin-amd64 ;;
  Darwin/arm64|Darwin/aarch64) target=darwin-arm64 ;;
  *) fail "unsupported platform $os/$arch" ;;
esac

tmpdir=$(mktemp -d 2>/dev/null || mktemp -d -t bacli)
trap 'rm -rf "$tmpdir"' EXIT HUP INT TERM

curl -fsSL "$MANIFEST_URL" -o "$tmpdir/version.json"
eval "$(python3 - "$tmpdir/version.json" "$target" "$MANIFEST_URL" <<'PY'
import json, shlex, sys
from urllib.parse import urljoin
p, target, manifest_url = sys.argv[1:]
data = json.load(open(p, encoding='utf-8'))
if data.get('schemaVersion') != 1:
    raise SystemExit('unsupported bacli release manifest')
entry = data.get('files', {}).get(target)
if not entry or not entry.get('url') or len(entry.get('sha256', '')) != 64:
    raise SystemExit('release manifest has no artifact for ' + target)
print('artifact_url=' + shlex.quote(urljoin(manifest_url, entry['url'])))
print('artifact_sha256=' + shlex.quote(entry['sha256'].lower()))
print('release_version=' + shlex.quote(str(data.get('version', 'unknown'))))
PY
)"

curl -fL "$artifact_url" -o "$tmpdir/bacli"
actual=$(python3 - "$tmpdir/bacli" <<'PY'
import hashlib, sys
h = hashlib.sha256()
with open(sys.argv[1], 'rb') as f:
    for chunk in iter(lambda: f.read(1024 * 1024), b''):
        h.update(chunk)
print(h.hexdigest())
PY
)
[ "$actual" = "$artifact_sha256" ] || fail "checksum verification failed"

mkdir -p "$INSTALL_DIR"
chmod 755 "$INSTALL_DIR"
chmod 755 "$tmpdir/bacli"
mv "$tmpdir/bacli" "$INSTALL_DIR/bacli"
chmod 755 "$INSTALL_DIR/bacli"

path_line='export PATH="$HOME/.local/bin:$PATH"'
case ${SHELL:-} in
  */zsh) rc="$HOME/.zshrc" ;;
  */bash) rc="$HOME/.bashrc" ;;
  *) rc="$HOME/.profile" ;;
esac
if ! printf '%s' ":$PATH:" | grep -q ":$HOME/.local/bin:"; then
  touch "$rc"
  if ! grep -Fqx "$path_line" "$rc"; then
    printf '\n# Added by bacli installer\n%s\n' "$path_line" >> "$rc"
  fi
  export PATH="$HOME/.local/bin:$PATH"
  printf 'Added ~/.local/bin to PATH in %s. Open a new shell or run: %s\n' "$rc" "$path_line"
fi

printf 'Installed bacli %s to %s/bacli\n' "$release_version" "$INSTALL_DIR"
