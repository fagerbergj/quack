#!/bin/sh
set -eu
AGENTS_DIR="$(cd "$(dirname "$0")" && pwd)"
"$AGENTS_DIR/setup-symlinks.sh"
"$AGENTS_DIR/install-plugins.sh"
