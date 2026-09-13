#!/bin/sh
#
# Install docket. No Go toolchain required.
#
#   curl -fsSL https://raw.githubusercontent.com/Dillonsmart/docket/main/install.sh | sh
#
# Environment:
#   DOCKET_VERSION      version to install (default: the latest release)
#   DOCKET_INSTALL_DIR  where to put the binary (default: /usr/local/bin, else ~/.local/bin)
#   DOCKET_DRY_RUN      set to 1 to print what would happen and exit
#
set -eu

REPO="Dillonsmart/docket"
BIN="docket"

say() { printf '%s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

need() {
	command -v "$1" >/dev/null 2>&1 || die "this installer needs $1"
}

detect_platform() {
	os=$(uname -s | tr '[:upper:]' '[:lower:]')
	arch=$(uname -m)
	case "$os" in
		darwin) ;;
		linux) ;;
		msys*|mingw*|cygwin*)
			die "on Windows, download the .zip from https://github.com/$REPO/releases/latest" ;;
		*) die "unsupported operating system: $os" ;;
	esac
	case "$arch" in
		x86_64|amd64) arch=amd64 ;;
		arm64|aarch64) arch=arm64 ;;
		*) die "unsupported architecture: $arch. Build from source with: go install github.com/$REPO/cmd/docket@latest" ;;
	esac
	printf '%s_%s' "$os" "$arch"
}

latest_version() {
	# The releases/latest URL redirects to the tag, which avoids depending on a
	# JSON parser being installed.
	url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest") ||
		die "could not reach GitHub to find the latest release"
	version=${url##*/}
	[ -n "$version" ] && [ "$version" != "latest" ] || die "no published release yet; install with: go install github.com/$REPO/cmd/docket@latest"
	printf '%s' "$version"
}

install_dir() {
	if [ -n "${DOCKET_INSTALL_DIR:-}" ]; then
		printf '%s' "$DOCKET_INSTALL_DIR"
	elif [ -w /usr/local/bin ] 2>/dev/null; then
		printf '/usr/local/bin'
	else
		printf '%s/.local/bin' "$HOME"
	fi
}

need curl
need tar

PLATFORM=$(detect_platform)
VERSION=${DOCKET_VERSION:-$(latest_version)}
ASSET="${BIN}_${VERSION}_${PLATFORM}.tar.gz"
URL="https://github.com/$REPO/releases/download/$VERSION/$ASSET"
SUMS="https://github.com/$REPO/releases/download/$VERSION/SHA256SUMS"
DEST=$(install_dir)

if [ "${DOCKET_DRY_RUN:-}" = "1" ]; then
	say "platform  $PLATFORM"
	say "version   $VERSION"
	say "asset     $URL"
	say "install   $DEST/$BIN"
	exit 0
fi

TMP=$(mktemp -d 2>/dev/null || mktemp -d -t docket)
trap 'rm -rf "$TMP"' EXIT INT TERM

say "downloading docket $VERSION for $PLATFORM"
curl -fsSL "$URL" -o "$TMP/$ASSET" || die "could not download $URL"

# Verify before unpacking. A checksum nobody checks is decoration.
if curl -fsSL "$SUMS" -o "$TMP/SHA256SUMS" 2>/dev/null; then
	expected=$(grep " $ASSET\$" "$TMP/SHA256SUMS" | awk '{print $1}')
	if [ -n "$expected" ]; then
		if command -v shasum >/dev/null 2>&1; then
			actual=$(shasum -a 256 "$TMP/$ASSET" | awk '{print $1}')
		elif command -v sha256sum >/dev/null 2>&1; then
			actual=$(sha256sum "$TMP/$ASSET" | awk '{print $1}')
		fi
		if [ -n "${actual:-}" ] && [ "$actual" != "$expected" ]; then
			die "checksum mismatch for $ASSET: expected $expected, got $actual"
		fi
	fi
else
	say "warning: no SHA256SUMS published for $VERSION; skipping verification"
fi

tar -xzf "$TMP/$ASSET" -C "$TMP"
SRC=$(find "$TMP" -type f -name "$BIN" -perm -u+x | head -n 1)
[ -n "$SRC" ] || die "the archive did not contain a $BIN binary"

mkdir -p "$DEST"
if ! mv "$SRC" "$DEST/$BIN" 2>/dev/null; then
	say "installing to $DEST needs elevated permissions"
	sudo mv "$SRC" "$DEST/$BIN" || die "could not install to $DEST"
fi
chmod +x "$DEST/$BIN" 2>/dev/null || sudo chmod +x "$DEST/$BIN"

say "installed $("$DEST/$BIN" version) to $DEST/$BIN"

case ":$PATH:" in
	*":$DEST:"*) ;;
	*) say ""
	   say "$DEST is not on your PATH. Add this to your shell profile:"
	   say "  export PATH=\"$DEST:\$PATH\"" ;;
esac

say ""
say "Next: cd into a repository and run"
say "  docket init"
