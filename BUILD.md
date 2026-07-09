# Build

Build Agent Go CLI release binaries are built as a single static, stripped Linux aarch64 executable named `build-agent-go-cli`.

## Target

- OS: Linux
- Architecture: aarch64 / arm64
- Linking: static
- Symbols/debug info: stripped
- Output filename: `build-agent-go-cli`

Do **not** build or publish dynamic non-stripped binaries for normal use. Only make a non-stripped build as a temporary local debugging exception when explicitly requested.

## Release build command

Run from the repository root:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w -buildid=' -o build-agent-go-cli .
```

Why these flags:

- `CGO_ENABLED=0` forces a pure-Go/static binary.
- `GOOS=linux GOARCH=arm64` targets Linux aarch64.
- `-trimpath` removes local filesystem paths from the binary.
- `-ldflags='-s -w -buildid='` strips symbols/debug metadata and removes the Go build id for a smaller, cleaner artifact.
- `-o build-agent-go-cli` keeps the release artifact name stable with no architecture suffix.

## Verification

After building, verify the artifact:

```bash
file build-agent-go-cli
ldd build-agent-go-cli || true
sha256sum build-agent-go-cli
ls -lh build-agent-go-cli
```

Expected results:

- `file` reports an `ELF 64-bit ... ARM aarch64` executable.
- `file` includes `statically linked, stripped`.
- `ldd` prints `not a dynamic executable`.

## Optional pre-build checks

Before producing a release binary, run:

```bash
go test -count=1 ./...
go build ./...
go vet ./...
```
