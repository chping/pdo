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


pdo_has_downloader=0
if command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1; then pdo_has_downloader=1; fi
pdo_has_hash=0
if command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1; then pdo_has_hash=1; fi

if [ "$pdo_os" = "darwin" ]; then
  pdo_missing=""
  command -v curl >/dev/null 2>&1 || pdo_missing="$pdo_missing curl"
  command -v awk >/dev/null 2>&1 || pdo_missing="$pdo_missing awk"
  command -v shasum >/dev/null 2>&1 || pdo_missing="$pdo_missing shasum"
  if [ -n "$pdo_missing" ]; then
    echo "pdo: missing macOS system components:$pdo_missing; repair or reinstall macOS Command Line Tools" >&2
    exit 1
  fi
elif [ "$pdo_has_downloader" -eq 0 ] || [ "$pdo_has_hash" -eq 0 ] || ! command -v awk >/dev/null 2>&1; then
  pdo_openwrt=0
  if [ -f /etc/openwrt_release ] || { [ -f /etc/os-release ] && grep -Eq '^ID="?openwrt"?$' /etc/os-release; }; then
    pdo_openwrt=1
  fi
  pdo_manager=""
  if [ "$pdo_openwrt" -eq 1 ]; then
    for pdo_candidate in opkg apk; do
      if command -v "$pdo_candidate" >/dev/null 2>&1; then pdo_manager=$pdo_candidate; break; fi
    done
  else
    for pdo_candidate in apt-get dnf yum pacman apk zypper; do
      if command -v "$pdo_candidate" >/dev/null 2>&1; then pdo_manager=$pdo_candidate; break; fi
    done
  fi
  if [ -z "$pdo_manager" ]; then
    echo "pdo: bootstrap dependencies are missing and no supported package manager was found" >&2
    exit 1
  fi

  pdo_packages=""
  [ "$pdo_has_downloader" -eq 1 ] || pdo_packages="$pdo_packages curl"
  if [ "$pdo_has_hash" -eq 0 ]; then
    if [ "$pdo_openwrt" -eq 1 ]; then pdo_packages="$pdo_packages coreutils-sha256sum"; else pdo_packages="$pdo_packages coreutils"; fi
  fi
  command -v awk >/dev/null 2>&1 || pdo_packages="$pdo_packages gawk"

  pdo_prefix=""
  if [ "$(id -u)" != "0" ]; then
    if ! command -v sudo >/dev/null 2>&1; then
      echo "pdo: sudo was not found; run the following as root:" >&2
    else
      pdo_prefix="sudo "
    fi
  fi
  case "$pdo_manager" in
    apt-get) pdo_commands="${pdo_prefix}apt-get update
${pdo_prefix}apt-get install -y$pdo_packages" ;;
    dnf|yum) pdo_commands="${pdo_prefix}${pdo_manager} install -y$pdo_packages" ;;
    pacman) pdo_commands="${pdo_prefix}pacman -Sy --needed --noconfirm$pdo_packages" ;;
    apk) pdo_commands="${pdo_prefix}apk add$pdo_packages" ;;
    zypper) pdo_commands="${pdo_prefix}zypper --non-interactive install$pdo_packages" ;;
    opkg) pdo_commands="${pdo_prefix}opkg update
${pdo_prefix}opkg install$pdo_packages" ;;
  esac
  echo "pdo: missing bootstrap dependencies. Install with:" >&2
  printf '%s\n' "$pdo_commands" | while IFS= read -r pdo_line; do echo "  $pdo_line" >&2; done
  if [ "$(id -u)" != "0" ] && [ -z "$pdo_prefix" ]; then exit 1; fi
  if ! { (: </dev/tty) 2>/dev/null && (: >/dev/tty) 2>/dev/null; }; then
    echo "pdo: non-interactive installation cannot install missing dependencies" >&2
    exit 1
  fi
  printf 'Install missing dependencies now? [y/N] ' >/dev/tty
  IFS= read -r pdo_answer </dev/tty || pdo_answer=""
  case "$pdo_answer" in
    [Yy]|[Yy][Ee][Ss]) ;;
    *) echo "pdo: dependency installation declined" >&2; exit 1 ;;
  esac
  case "$pdo_manager" in
    apt-get) ${pdo_prefix}apt-get update && ${pdo_prefix}apt-get install -y $pdo_packages ;;
    dnf|yum) ${pdo_prefix}$pdo_manager install -y $pdo_packages ;;
    pacman) ${pdo_prefix}pacman -Sy --needed --noconfirm $pdo_packages ;;
    apk) ${pdo_prefix}apk add $pdo_packages ;;
    zypper) ${pdo_prefix}zypper --non-interactive install $pdo_packages ;;
    opkg) ${pdo_prefix}opkg update && ${pdo_prefix}opkg install $pdo_packages ;;
  esac
fi

if { ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; } || \
   { ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; } || \
   ! command -v awk >/dev/null 2>&1; then
  echo "pdo: bootstrap dependencies are still missing after installation" >&2
  exit 1
fi

pdo_download() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$2"
  else
    wget -q -O "$2" "$1"
  fi
}

pdo_asset="pdo_${pdo_os}_${pdo_arch}"
pdo_base_url="https://github.com/${pdo_repo}/releases/latest/download"
pdo_temp_dir=$(mktemp -d)
trap 'rm -rf "$pdo_temp_dir"' EXIT HUP INT TERM

pdo_download "$pdo_base_url/$pdo_asset" "$pdo_temp_dir/$pdo_asset"
pdo_download "$pdo_base_url/SHA256SUMS" "$pdo_temp_dir/SHA256SUMS"

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

chmod 755 "$pdo_temp_dir/$pdo_asset"
if (: </dev/tty) 2>/dev/null && (: >/dev/tty) 2>/dev/null; then
  "$pdo_temp_dir/$pdo_asset" __pdo-dependencies </dev/tty >/dev/tty 2>/dev/tty
else
  "$pdo_temp_dir/$pdo_asset" __pdo-dependencies
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
