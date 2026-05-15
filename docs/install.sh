#!/usr/bin/env bash
# radii5 installer - short URL entry point
# Usage: curl -fsSL https://ohcass.github.io/music/install.sh | sh
set -euo pipefail
exec bash <(curl -fsSL https://raw.githubusercontent.com/ohcass/music/main/scripts/install.sh)
