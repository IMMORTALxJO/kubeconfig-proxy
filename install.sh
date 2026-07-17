#!/bin/sh
set -eu

repo="IMMORTALxJO/kubeconfig-proxy"
binary="kubeconfig-proxy"
install_dir="${INSTALL_DIR:-/usr/local/bin}"
version="${KUBECONFIG_PROXY_VERSION:-latest}"

log() {
	printf '%s\n' "$*"
}

fail() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

need_cmd() {
	command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

detect_os() {
	case "$(uname -s)" in
		Linux) printf 'linux' ;;
		Darwin) printf 'darwin' ;;
		*) fail "unsupported OS: $(uname -s)" ;;
	esac
}

detect_arch() {
	case "$(uname -m)" in
		x86_64 | amd64) printf 'amd64' ;;
		arm64 | aarch64) printf 'arm64' ;;
		*) fail "unsupported architecture: $(uname -m)" ;;
	esac
}

resolve_latest_version() {
	latest_url="$(curl -fsSIL -o /dev/null -w '%{url_effective}' "https://github.com/${repo}/releases/latest")"
	printf '%s' "$latest_url" | sed 's#.*/##'
}

verify_checksum() {
	archive="$1"
	checksums="$2"
	asset="$3"

	expected="$(awk -v asset="$asset" '$2 == asset {print $1}' "$checksums")"
	[ -n "$expected" ] || fail "checksum for ${asset} not found"

	if command -v sha256sum >/dev/null 2>&1; then
		actual="$(sha256sum "$archive" | awk '{print $1}')"
	elif command -v shasum >/dev/null 2>&1; then
		actual="$(shasum -a 256 "$archive" | awk '{print $1}')"
	else
		fail "sha256sum or shasum is required"
	fi

	[ "$actual" = "$expected" ] || fail "checksum mismatch for ${asset}"
}

install_binary() {
	src="$1"
	dst="${install_dir}/${binary}"

	if [ ! -d "$install_dir" ]; then
		if mkdir -p "$install_dir" 2>/dev/null; then
			:
		elif command -v sudo >/dev/null 2>&1; then
			sudo mkdir -p "$install_dir"
		else
			fail "could not create ${install_dir}; rerun with INSTALL_DIR set to a writable directory"
		fi
	fi

	if [ -d "$install_dir" ] && [ -w "$install_dir" ]; then
		install -m 0755 "$src" "$dst"
		return
	fi

	if command -v sudo >/dev/null 2>&1; then
		sudo install -m 0755 "$src" "$dst"
		return
	fi

	fail "${install_dir} is not writable; rerun with INSTALL_DIR set to a writable directory"
}

need_cmd curl
need_cmd awk
need_cmd sed
need_cmd tar
need_cmd install
need_cmd find
need_cmd mkdir
need_cmd mktemp

os="$(detect_os)"
arch="$(detect_arch)"

if [ "$version" = "latest" ]; then
	version="$(resolve_latest_version)"
fi

case "$version" in
	v*) ;;
	*) version="v${version}" ;;
esac

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT INT TERM

asset="${binary}_${version}_${os}_${arch}.tar.gz"
base_url="https://github.com/${repo}/releases/download/${version}"
archive="${tmp_dir}/${asset}"
checksums="${tmp_dir}/checksums.txt"

log "Installing ${binary} ${version} for ${os}/${arch}"
curl -fsSL "${base_url}/${asset}" -o "$archive"
curl -fsSL "${base_url}/checksums.txt" -o "$checksums"
verify_checksum "$archive" "$checksums" "$asset"

tar -xzf "$archive" -C "$tmp_dir"
src="$(find "$tmp_dir" -type f -name "$binary" -perm -u+x | head -n 1)"
[ -n "$src" ] || fail "${binary} not found in ${asset}"

install_binary "$src"
log "Installed ${binary} to ${install_dir}/${binary}"
