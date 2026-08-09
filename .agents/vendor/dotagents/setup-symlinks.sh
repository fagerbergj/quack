#!/bin/sh
# Symlink this repo (cloned at ~/.agents) into each harness's config location.
# Safe to re-run: replaces stale symlinks, backs up real files to <path>.bak.
set -eu

AGENTS_DIR="$(cd "$(dirname "$0")" && pwd)"


link() {
  src="$1" dst="$2"
  mkdir -p "$(dirname "$dst")"
  if [ -L "$dst" ]; then
    rm "$dst"
  elif [ -e "$dst" ]; then
    echo "backing up $dst -> $dst.bak"
    mv "$dst" "$dst.bak"
  fi
  ln -s "$src" "$dst"
  echo "linked $dst -> $src"
}

# Claude Code
link "$AGENTS_DIR/AGENTS.md" "$HOME/.claude/CLAUDE.md"
link "$AGENTS_DIR/AGENTS.md" "$HOME/.claude/AGENTS.md"
link "$AGENTS_DIR/skills" "$HOME/.claude/skills"

# opencode (global rules; skills are read from ~/.agents/skills natively)
link "$AGENTS_DIR/AGENTS.md" "$HOME/.config/opencode/AGENTS.md"

# pi
link "$AGENTS_DIR/AGENTS.md" "$HOME/.pi/agent/AGENTS.md"
link "$AGENTS_DIR/skills" "$HOME/.pi/agent/skills"

# codex
link "$AGENTS_DIR/AGENTS.md" "$HOME/.codex/AGENTS.md"
