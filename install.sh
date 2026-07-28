#!/bin/sh
# install.sh — fetch the right vip-index-supervisor release binary and put it
# on PATH. Safe to pipe: nothing executes until the whole script has arrived,
# because everything lives in main(), invoked only on the final line.
#
#   curl -fsSL https://raw.githubusercontent.com/JDeepD/vip-index-supervisor/main/install.sh | sh
#
# Options via environment variables:
#   VERSION      release tag to install (default: latest)
#   INSTALL_DIR  target directory (default: /usr/local/bin if writable,
#                otherwise ~/.local/bin)
set -eu

REPO="${REPO:-JDeepD/vip-index-supervisor}"
BASE_URL="${BASE_URL:-https://github.com/${REPO}/releases}"
NAME="vip-index-supervisor"

say() { printf '%s\n' "$*" >&2; }
die() { say "error: $*"; exit 1; }

detect_asset() {
	os=$(uname -s | tr '[:upper:]' '[:lower:]')
	arch=$(uname -m)
	case "$arch" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported architecture: $arch" ;;
	esac
	case "$os" in
	linux | darwin) ;;
	*) die "unsupported OS: $os (on Windows, download ${NAME}-windows-amd64.exe from the releases page)" ;;
	esac
	printf '%s-%s-%s' "$NAME" "$os" "$arch"
}

release_url() {
	if [ "${VERSION:-latest}" = "latest" ]; then
		printf '%s/latest/download/%s' "$BASE_URL" "$1"
	else
		printf '%s/download/%s/%s' "$BASE_URL" "$VERSION" "$1"
	fi
}

fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$2" "$1"
	else
		die "neither curl nor wget is available"
	fi
}

verify_checksum() {
	# checksums.txt ships in the same release; this catches a corrupted or
	# truncated download, which is the failure mode piping can hide.
	dir=$1 asset=$2
	if ! fetch "$(release_url checksums.txt)" "$dir/checksums.txt"; then
		say "warning: checksums.txt not found in the release — skipping verification"
		return 0
	fi
	expected=$(grep " $asset\$" "$dir/checksums.txt" | cut -d' ' -f1)
	[ -n "$expected" ] || die "no checksum listed for $asset"
	if command -v sha256sum >/dev/null 2>&1; then
		actual=$(sha256sum "$dir/$asset" | cut -d' ' -f1)
	else
		actual=$(shasum -a 256 "$dir/$asset" | cut -d' ' -f1)
	fi
	[ "$actual" = "$expected" ] || die "checksum mismatch for $asset — download corrupted, try again"
}

pick_install_dir() {
	if [ -n "${INSTALL_DIR:-}" ]; then
		printf '%s' "$INSTALL_DIR"
	elif [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
		printf '/usr/local/bin'
	else
		printf '%s/.local/bin' "$HOME"
	fi
}

main() {
	asset=$(detect_asset)
	install_dir=$(pick_install_dir)
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT

	say "downloading $asset (${VERSION:-latest})..."
	fetch "$(release_url "$asset")" "$tmp/$asset" || die "download failed — does the release exist?"
	verify_checksum "$tmp" "$asset"

	mkdir -p "$install_dir"
	chmod +x "$tmp/$asset"
	mv "$tmp/$asset" "$install_dir/$NAME"
	say "installed $install_dir/$NAME"

	case ":$PATH:" in
	*":$install_dir:"*) ;;
	*) say "note: $install_dir is not on PATH — add it, or run $install_dir/$NAME directly" ;;
	esac
}

main "$@"
