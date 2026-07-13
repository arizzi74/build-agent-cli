#!/bin/sh
set -eu

cd "$(dirname "$0")/.."

VERSION=${VERSION:-$(date -u +%Y.%m.%d).$(git rev-parse --short=8 HEAD)}
BASE_URL=${BASE_URL:-https://nowdemo.it/bacli}
DIST=dist
if [ -x /usr/local/go/bin/go ]; then
  GO=${GO:-/usr/local/go/bin/go}
else
  GO=${GO:-go}
fi
LDFLAGS="-s -w -buildid= -X main.cliVersion=$VERSION -X main.cliUpdateBaseURL=$BASE_URL"

mkdir -p "$DIST"
rm -f "$DIST"/bacli-linux-arm64 "$DIST"/bacli-darwin-amd64 "$DIST"/bacli-darwin-arm64 "$DIST"/bacli-windows-amd64.exe "$DIST"/version.json

build() {
  goos=$1
  goarch=$2
  output=$3
  printf 'Building %s/%s -> %s\n' "$goos" "$goarch" "$output"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" "$GO" build -trimpath -ldflags="$LDFLAGS" -o "$DIST/$output" .
}

build linux arm64 bacli-linux-arm64
build darwin amd64 bacli-darwin-amd64
build darwin arm64 bacli-darwin-arm64
build windows amd64 bacli-windows-amd64.exe
chmod 755 "$DIST/install.sh" "$DIST/bacli-linux-arm64" "$DIST/bacli-darwin-amd64" "$DIST/bacli-darwin-arm64"

python3 - "$VERSION" "$BASE_URL" "$DIST" <<'PY'
import hashlib, json, pathlib, sys
version, base, dist = sys.argv[1:]
d = pathlib.Path(dist)
files = {
    'linux-arm64': 'bacli-linux-arm64',
    'darwin-amd64': 'bacli-darwin-amd64',
    'darwin-arm64': 'bacli-darwin-arm64',
    'windows-amd64': 'bacli-windows-amd64.exe',
}
out = {'schemaVersion': 1, 'version': version, 'files': {}}
for target, name in files.items():
    raw = (d / name).read_bytes()
    out['files'][target] = {'url': name, 'sha256': hashlib.sha256(raw).hexdigest()}
(d / 'version.json').write_text(json.dumps(out, indent=2) + '\n', encoding='utf-8')
PY

printf 'Release %s built in %s/\n' "$VERSION" "$DIST"
sha256sum "$DIST"/bacli-* "$DIST"/install.sh "$DIST"/install.ps1 "$DIST"/version.json > "$DIST/SHA256SUMS"
