#!/usr/bin/env bash
# Proves this checkout's modules still build, test and validate inside quack, before anything is tagged.
# Usage: tools/quack-compat.sh [quack-dir]   (no arg: shallow-clones fagerbergj/quack at $QUACK_REF, default main)
set -euo pipefail

ext=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
step=setup
infra=
on_exit() {
	local rc=$?
	rm -rf "$tmp"
	[ "$rc" -eq 0 ] && return
	if [ -n "$infra" ]; then
		echo "::error::quack-compat could not run \"$step\" (network, or a bad QUACK_REF/quack dir). This is not a verdict on the change; rerun." >&2
	else
		echo "::error::quack-compat failed at \"$step\" against quack ${quack_desc:-?}: this change breaks quack. Fix the module, or if quack must change with it, see \"Checking a change against quack\" in README.md." >&2
	fi
}
trap on_exit EXIT
run() { step=$*; echo "== $step"; "$@"; }

quack=${1:-$tmp/quack}
infra=1
[ -n "${1:-}" ] || run git clone -q --depth 1 --branch "${QUACK_REF:-main}" https://github.com/fagerbergj/quack "$quack"
cd "$quack"
quack_desc="$(git rev-parse --abbrev-ref HEAD)@$(git rev-parse --short HEAD) ($PWD)"
echo "== quack: $quack_desc"

# GOWORK outside quack's tree: that checkout's own go commands stay on its go.mod pins.
export GOWORK=$tmp/go.work
mods=("$ext"/*/go.mod)
run go work init . "${mods[@]%/go.mod}"
run go mod download
infra=
run go build ./...
# vet type-checks every quack test file against these modules.
run go vet ./...

step="list quack packages depending on quack-extensions"
pkgs=$(go list -f '{{.ImportPath}} {{join .Deps " "}} {{join .TestImports " "}} {{join .XTestImports " "}}' ./... | awk '/quack-extensions\//{print $1}')
[ -n "$pkgs" ] || { echo "no quack package depends on quack-extensions; the go.work is wrong" >&2; exit 1; }
# shellcheck disable=SC2086 # one import path per word
run go test -count=1 $pkgs

run go build -o "$tmp/quack-bin" ./cmd/quack
# Placeholder env for quack's shipped config, read as KEY=VALUE data rather than sourced as shell.
while IFS='=' read -r k v; do
	if [[ $k =~ ^[A-Z_][A-Z0-9_]*$ ]]; then export "$k=$v"; fi
done <.env.example
export QUACK_MEDIA_MODEL=${QUACK_MEDIA_MODEL:-qwen3-omni-30b} QUACK_IMAGE_MODEL=${QUACK_IMAGE_MODEL:-qwen3-vl-32b}
run "$tmp/quack-bin" server validate config/quack.yaml

export QUACK_COMPAT_TMP=$tmp QUACK_COMPAT_GITHUB_KEY=$tmp/github.pem QUACK_COMPAT_SECRET=placeholder
run openssl genrsa -out "$QUACK_COMPAT_GITHUB_KEY" 2048
# The fixture seeds each module's plugin/ from this checkout, the way quack seeds github:...#<module>/plugin.
step="copy each module's plugin/ into the fixture"
shopt -s nullglob
plugins=("$ext"/*/plugin/plugin.json)
[ ${#plugins[@]} -gt 0 ] || { echo "no */plugin/plugin.json in $ext; the fixture's plugins.seed would name nothing" >&2; exit 1; }
mkdir -p "$tmp/plugins"
for p in "${plugins[@]%/plugin.json}"; do
	cp -R "$p" "$tmp/plugins/$(basename "$(dirname "$p")")"
done
# A tool without its module's prefix must be a quack builtin; this catches quack renaming or removing one.
for p in "${plugins[@]}"; do
	module=$(basename "$(dirname "$(dirname "$p")")")
	for y in "$(dirname "$p")"/agents/*/agent.yaml; do
		step="builtin tools named in $y exist in quack"
		tools=$(sed -n 's/^tools:[[:space:]]*\[\(.*\)\][[:space:]]*$/\1/p' "$y" | tr ',' ' ')
		# No tools: key is fine (github's ACP agents get theirs from quack); a multi-line list would go unchecked.
		if [ -z "$tools" ] && grep -q '^tools:' "$y"; then
			echo "$y: expected a one-line tools: [...] list" >&2
			exit 1
		fi
		for t in $tools; do
			[[ $t == "${module}_"* ]] && continue
			grep -q "\"$t\":" internal/tools/registry.go || { echo "$y: tool \"$t\" is not in quack's internal/tools/registry.go builtin map" >&2; exit 1; }
		done
	done
done
shopt -u nullglob
step="quack server validate tools/quack-compat.config.yaml"
echo "== $step"
validated=$("$tmp/quack-bin" server validate --json "$ext/tools/quack-compat.config.yaml")
accepted=$(jq -r '.extensions[]?' <<<"$validated")
echo "accepted: ${accepted//$'\n'/ }"
# A plugin seeds only what its manifest lists; anything short of the full lists means quack refused or skipped part of it.
for p in "${plugins[@]}"; do
	name=$(basename "$(dirname "$(dirname "$p")")")
	step="plugin $name seeds every listed agent and workflow"
	want=$(jq -c '.extensions["io.github.fagerbergj.quack"] | [(.agents // [] | sort), (.workflows // [] | sort)]' "$p")
	[ "$want" = '[[],[]]' ] && continue
	got=$(jq -c --arg n "$name" '[.plugins[]? | select(.plugin == $n) | [(.agents // [] | sort), (.shapes // [] | sort)]][0] // "not seeded"' <<<"$validated")
	[ "$want" = "$got" ] || { echo "$name: manifest lists $want, quack seeded $got" >&2; exit 1; }
	echo "plugin $name: seeded $got"
done
# Every extension module needs a fixture block, or its Factory goes unchecked.
for m in "${mods[@]%/go.mod}"; do
	name=${m##*/}
	[ "$name" = sdk ] && continue
	step="fixture covers $name"
	grep -qx "$name" <<<"$accepted" || { echo "$name: no accepted extensions.$name block in tools/quack-compat.config.yaml" >&2; exit 1; }
done
echo "quack-compat: OK against quack $quack_desc"
