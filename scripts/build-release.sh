#!/usr/bin/env bash
#
# Cross-compile docket for every platform we ship, package each build and write
# a checksum file. Run locally or from the release workflow; both paths go
# through this script so a release is never the first time it runs.
#
#   scripts/build-release.sh [version] [output directory]
#
set -euo pipefail

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
OUT="${2:-dist}"

# The platforms a person is plausibly running an agent on. Adding one is a line
# here and a line in install.sh.
PLATFORMS=(
	"darwin/arm64"
	"darwin/amd64"
	"linux/amd64"
	"linux/arm64"
	"windows/amd64"
	"windows/arm64"
)

rm -rf "$OUT"
mkdir -p "$OUT"

for platform in "${PLATFORMS[@]}"; do
	GOOS="${platform%/*}"
	GOARCH="${platform#*/}"
	name="docket_${VERSION}_${GOOS}_${GOARCH}"
	bin="docket"
	[ "$GOOS" = "windows" ] && bin="docket.exe"

	workdir="$OUT/$name"
	mkdir -p "$workdir"

	# Static, reproducible-ish builds: no cgo, no absolute paths in the binary,
	# and the version stamped in so `docket version` and every record it writes
	# agree with the tag.
	CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
		go build -trimpath \
		-ldflags "-s -w -X main.version=${VERSION}" \
		-o "$workdir/$bin" ./cmd/docket

	cp LICENSE NOTICE README.md "$workdir/"

	if [ "$GOOS" = "windows" ]; then
		(cd "$OUT" && zip -qr "${name}.zip" "$name")
	else
		tar -czf "$OUT/${name}.tar.gz" -C "$OUT" "$name"
	fi
	rm -rf "$workdir"
	echo "built $name"
done

(cd "$OUT" && shasum -a 256 docket_* > SHA256SUMS 2>/dev/null || sha256sum docket_* > SHA256SUMS)
echo
echo "$OUT:"
ls -1 "$OUT"
