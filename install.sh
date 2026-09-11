#!/bin/sh
set -eu

cpdo_repo="chping/cpdo"
cpdo_os=$(uname -s)
cpdo_arch=$(uname -m)

case "$cpdo_os" in
  Darwin) cpdo_os="darwin" ;;
  Linux) cpdo_os="linux" ;;
  *) echo "cpdo: unsupported operating system: $cpdo_os" >&2; exit 1 ;;
esac

case "$cpdo_arch" in
  x86_64|amd64) cpdo_arch="amd64" ;;
  arm64|aarch64) cpdo_arch="arm64" ;;
  *) echo "cpdo: unsupported architecture: $cpdo_arch" >&2; exit 1 ;;
esac

if ! command -v curl >/dev/null 2>&1; then
  echo "cpdo: curl is required" >&2
  exit 1
fi

cpdo_asset="cpdo_${cpdo_os}_${cpdo_arch}"
cpdo_base_url="https://github.com/${cpdo_repo}/releases/latest/download"
cpdo_temp_dir=$(mktemp -d)
trap 'rm -rf "$cpdo_temp_dir"' EXIT HUP INT TERM

curl -fsSL "$cpdo_base_url/$cpdo_asset" -o "$cpdo_temp_dir/$cpdo_asset"
curl -fsSL "$cpdo_base_url/SHA256SUMS" -o "$cpdo_temp_dir/SHA256SUMS"

cpdo_expected=$(awk -v file="$cpdo_asset" '$2 == file || $2 == "*" file {print $1; exit}' "$cpdo_temp_dir/SHA256SUMS")
if [ -z "$cpdo_expected" ]; then
  echo "cpdo: checksum not found for $cpdo_asset" >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  cpdo_actual=$(sha256sum "$cpdo_temp_dir/$cpdo_asset" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  cpdo_actual=$(shasum -a 256 "$cpdo_temp_dir/$cpdo_asset" | awk '{print $1}')
else
  echo "cpdo: sha256sum or shasum is required" >&2
  exit 1
fi
if [ "$cpdo_expected" != "$cpdo_actual" ]; then
  echo "cpdo: checksum verification failed" >&2
  exit 1
fi

cpdo_install_dir="$HOME/.local/bin"
mkdir -p "$cpdo_install_dir"
cpdo_staged="$cpdo_install_dir/.cpdo.$$.tmp"
cp "$cpdo_temp_dir/$cpdo_asset" "$cpdo_staged"
chmod 755 "$cpdo_staged"
mv -f "$cpdo_staged" "$cpdo_install_dir/cpdo"

cpdo_config_dir="$HOME/.config/cpdo"
cpdo_config="$cpdo_config_dir/config.json"
if [ ! -e "$cpdo_config" ] && [ ! -L "$cpdo_config" ]; then
  umask 077
  mkdir -p "$cpdo_config_dir"
  cat >"$cpdo_config" <<'EOF'
{
  "schema_version": 1,
  "github": {
    "pat_env": "CPDO_GITHUB_PAT"
  },
  "ssh_config": {
    "repository": "OWNER/REPOSITORY",
    "branch": "main",
    "path": "ssh_config"
  }
}
EOF
  echo "Created $cpdo_config"
fi

echo "Installed cpdo to $cpdo_install_dir/cpdo"
case ":$PATH:" in
  *":$cpdo_install_dir:"*) ;;
  *) echo "Add $cpdo_install_dir to PATH before running cpdo." ;;
esac
echo "Edit $cpdo_config and set CPDO_GITHUB_PAT in your environment."
