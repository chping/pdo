#!/bin/sh
set -eu

cpdo_binary="$HOME/.local/bin/cpdo"
if [ -e "$cpdo_binary" ] || [ -L "$cpdo_binary" ]; then
  rm -f "$cpdo_binary"
  echo "Removed $cpdo_binary"
else
  echo "cpdo is not installed at $cpdo_binary"
fi

echo "Kept configuration at $HOME/.config/cpdo/config.json"
