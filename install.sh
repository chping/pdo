#!/bin/sh
set -eu

pdo_repo="chping/pdo"
pdo_os=$(uname -s)
pdo_arch=$(uname -m)

case "$pdo_os" in
  Darwin) pdo_os="darwin" ;;
  Linux) pdo_os="linux" ;;
  *) echo "pdo: unsupported operating system: $pdo_os" >&2; exit 1 ;;
esac

case "$pdo_arch" in
  x86_64|amd64) pdo_arch="amd64" ;;
  arm64|aarch64) pdo_arch="arm64" ;;
  *) echo "pdo: unsupported architecture: $pdo_arch" >&2; exit 1 ;;
esac

if ! command -v curl >/dev/null 2>&1; then
  echo "pdo: curl is required" >&2
  exit 1
fi

pdo_asset="pdo_${pdo_os}_${pdo_arch}"
pdo_base_url="https://github.com/${pdo_repo}/releases/latest/download"
pdo_temp_dir=$(mktemp -d)
trap 'rm -rf "$pdo_temp_dir"' EXIT HUP INT TERM

curl -fsSL "$pdo_base_url/$pdo_asset" -o "$pdo_temp_dir/$pdo_asset"
curl -fsSL "$pdo_base_url/SHA256SUMS" -o "$pdo_temp_dir/SHA256SUMS"

pdo_expected=$(awk -v file="$pdo_asset" '$2 == file || $2 == "*" file {print $1; exit}' "$pdo_temp_dir/SHA256SUMS")
if [ -z "$pdo_expected" ]; then
  echo "pdo: checksum not found for $pdo_asset" >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  pdo_actual=$(sha256sum "$pdo_temp_dir/$pdo_asset" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  pdo_actual=$(shasum -a 256 "$pdo_temp_dir/$pdo_asset" | awk '{print $1}')
else
  echo "pdo: sha256sum or shasum is required" >&2
  exit 1
fi
if [ "$pdo_expected" != "$pdo_actual" ]; then
  echo "pdo: checksum verification failed" >&2
  exit 1
fi

pdo_install_dir="$HOME/.local/bin"
mkdir -p "$pdo_install_dir"
pdo_staged="$pdo_install_dir/.pdo.$$.tmp"
cp "$pdo_temp_dir/$pdo_asset" "$pdo_staged"
chmod 755 "$pdo_staged"
mv -f "$pdo_staged" "$pdo_install_dir/pdo"

pdo_config_dir="$HOME/.config/pdo"
pdo_config="$pdo_config_dir/config.json"
pdo_create_default_config() {
  if [ -e "$pdo_config" ] || [ -L "$pdo_config" ]; then
    return
  fi
  umask 077
  mkdir -p "$pdo_config_dir"
  cat >"$pdo_config" <<'EOF'
{
  "schema_version": 1,
  "cloud_clipboard": {
    "host": "https://clipboard.example.com/",
    "room": "personal",
    "password_env": "PDO_CLOUD_CLIPBOARD_PASSWORD"
  },
  "github": {
    "pat_env": "PDO_GITHUB_PAT"
  },
  "dotfiles": {
    "ssh-config": {
      "remote": "https://github.com/OWNER/REPOSITORY/blob/main/ssh_config",
      "local": "~/.ssh/config"
    }
  }
}
EOF
  echo "Created $pdo_config"
}

echo "Installed pdo to $pdo_install_dir/pdo"
case ":$PATH:" in
  *":$pdo_install_dir:"*) ;;
  *) echo "Add $pdo_install_dir to PATH before running pdo." ;;
esac

pdo_run_setup=0
if (: </dev/tty) 2>/dev/null && (: >/dev/tty) 2>/dev/null; then
  printf 'Run pdo setup now? [y/N] ' >/dev/tty
  if IFS= read -r pdo_answer </dev/tty; then
    case "$pdo_answer" in
      [Yy]|[Yy][Ee][Ss]) pdo_run_setup=1 ;;
    esac
  fi
fi
if [ "$pdo_run_setup" -eq 1 ]; then
  if ! "$pdo_install_dir/pdo" setup </dev/tty >/dev/tty 2>/dev/tty; then
    echo "pdo: setup failed; pdo remains installed at $pdo_install_dir/pdo" >&2
    exit 1
  fi
else
  pdo_create_default_config
  echo "Edit $pdo_config and set the configured password/token environment variables, or run pdo setup."
fi
