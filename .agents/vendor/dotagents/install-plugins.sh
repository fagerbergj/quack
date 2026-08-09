#!/bin/sh
# Install the plugins this setup expects. Safe to re-run.
# Fault tolerant: a missing harness CLI or a failed install warns and moves on.
set -eu

claude_install() {
  plugin="$1" marketplace="$2"
  if claude plugin list 2>/dev/null | grep -q "$plugin"; then
    echo "$plugin already installed"
    return 0
  fi
  claude plugin marketplace add "$marketplace" && claude plugin install "$plugin"
}

# Claude Code
if command -v claude >/dev/null 2>&1; then
  if ! claude_install ponytail@ponytail https://github.com/DietrichGebert/ponytail.git; then
    echo "warn: ponytail@ponytail install failed; inside Claude Code run: /plugin install ponytail@ponytail" >&2
  fi
else
  echo "warn: claude CLI not found; skipping Claude Code plugins" >&2
fi
