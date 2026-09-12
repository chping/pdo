#!/bin/sh
set -eu

pdo_binary="$HOME/.local/bin/pdo"
if [ -e "$pdo_binary" ] || [ -L "$pdo_binary" ]; then
  rm -f "$pdo_binary"
  echo "Removed $pdo_binary"
else
  echo "pdo is not installed at $pdo_binary"
fi

echo "Kept configuration at $HOME/.config/pdo/config.json"
