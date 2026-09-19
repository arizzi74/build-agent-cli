#!/bin/sh
set -eu

BASE_URL=${BACLI_BASE_URL:-https://github.com/arizzi74/build-agent-cli/releases/latest/download}
BASE_URL=${BASE_URL%/}
INSTALL_DIR=${BACLI_INSTALL_DIR:-"$HOME/.local/bin"}
MANIFEST_URL="$BASE_URL/version.json"

fail() {
  printf 'bacli installer: %s\n' "$*" >&2
  exit 1
}

command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v python3 >/dev/null 2>&1 || fail "python3 is required to read the release manifest"

os=$(uname -s)
arch=$(uname -m)
case "$os/$arch" in
  Linux/aarch64|Linux/arm64) target=linux-arm64 ;;
  Linux/x86_64|Linux/amd64) target=linux-amd64 ;;
  Darwin/x86_64|Darwin/amd64) target=darwin-amd64 ;;
  Darwin/arm64|Darwin/aarch64) target=darwin-arm64 ;;
  *) fail "unsupported platform $os/$arch" ;;
esac

tmpdir=$(mktemp -d 2>/dev/null || mktemp -d -t bacli)
trap 'rm -rf "$tmpdir"' EXIT HUP INT TERM

curl -fsSL "$MANIFEST_URL" -o "$tmpdir/version.json" || fail "could not download release manifest from $MANIFEST_URL"
manifest_vars=$(python3 - "$tmpdir/version.json" "$target" "$MANIFEST_URL" <<'PY'
import json, re, shlex, sys
from urllib.parse import urljoin, urlsplit
p, target, manifest_url = sys.argv[1:]
try:
    with open(p, encoding='utf-8') as f:
        data = json.load(f)
except (OSError, ValueError) as exc:
    raise SystemExit('invalid bacli release manifest: ' + str(exc))
if not isinstance(data, dict) or data.get('schemaVersion') != 1:
    raise SystemExit('unsupported bacli release manifest')
files = data.get('files')
entry = files.get(target) if isinstance(files, dict) else None
if not isinstance(entry, dict) or not isinstance(entry.get('url'), str) or not entry['url']:
    raise SystemExit('release manifest has no artifact for ' + target)
if not isinstance(entry.get('sha256'), str) or not re.fullmatch(r'[0-9a-fA-F]{64}', entry['sha256']):
    raise SystemExit('release manifest has an invalid checksum for ' + target)
artifact_url = urljoin(manifest_url, entry['url'])
parsed_url = urlsplit(artifact_url)
if parsed_url.scheme not in ('https', 'http') or not parsed_url.netloc:
    raise SystemExit('release manifest has an invalid artifact URL for ' + target)
print('artifact_url=' + shlex.quote(artifact_url))
print('artifact_sha256=' + shlex.quote(entry['sha256'].lower()))
print('release_version=' + shlex.quote(str(data.get('version', 'unknown'))))
PY
) || fail "could not read release manifest"
eval "$manifest_vars"

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
