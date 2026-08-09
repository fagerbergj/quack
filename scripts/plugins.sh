#!/usr/bin/env bash
# Fetch the plugin trees in .agents/vendor/plugins.yaml (not in git) to their
# pinned refs. --update moves each pin to remote HEAD; a name limits to one.
# Offline-tolerant: never deletes a tree it cannot re-fetch.
set -uo pipefail

cd "$(dirname "$0")/.."
MANIFEST=.agents/vendor/plugins.yaml
[[ -f $MANIFEST ]] || { echo "no $MANIFEST" >&2; exit 1; }

UPDATE=0
[[ ${1:-} == --update ]] && { UPDATE=1; shift; }
want=${1:-}

parse() {
  awk '
    /^  - name:/ { if (n != "") print n "\t" u "\t" r "\t" p; n=$3; u=r=p="" ; next }
    /^    url:/  { u=$2; next }
    /^    ref:/  { r=$2; next }
    /^    path:/ { p=$2; next }
    END { if (n != "") print n "\t" u "\t" r "\t" p }
  ' "$MANIFEST"
}

rc=0
while IFS=$'\t' read -r name url ref path; do
  [[ -n $want && $want != "$name" ]] && continue
  [[ -n $name && -n $url && -n $ref && -n $path ]] || { echo "manifest entry incomplete: $name" >&2; rc=1; continue; }

  stamp=$path/.plugin-ref
  if [[ $UPDATE -eq 0 && -f $stamp && $(cat "$stamp" 2>/dev/null) == "$ref" && -d $path ]]; then
    echo "$name: already at $ref"
    continue
  fi

  tmp=$(mktemp -d)
  if ! git clone --quiet "$url" "$tmp/c" 2>/dev/null; then
    rm -rf "$tmp"
    if [[ -d $path ]]; then
      echo "$name: clone failed, keeping the tree already on disk" >&2
      continue
    fi
    echo "$name: clone failed and no tree on disk" >&2
    rc=1; continue
  fi

  target=$ref
  if [[ $UPDATE -eq 1 ]]; then
    target=$(git -C "$tmp/c" rev-parse HEAD)
  elif ! git -C "$tmp/c" cat-file -e "$ref^{commit}" 2>/dev/null; then
    echo "$name: pinned ref $ref not found upstream" >&2
    rm -rf "$tmp"; rc=1; continue
  fi
  git -C "$tmp/c" checkout --quiet "$target" 2>/dev/null || {
    echo "$name: cannot check out $target" >&2; rm -rf "$tmp"; rc=1; continue; }
  resolved=$(git -C "$tmp/c" rev-parse HEAD)

  rm -rf "$tmp/c/.git"
  rm -rf "${path:?}"
  mkdir -p "$path"
  cp -a "$tmp/c/." "$path/"
  echo "$resolved" > "$stamp"
  rm -rf "$tmp"

  if [[ $UPDATE -eq 1 && $resolved != "$ref" ]]; then
    sed -i "s|^    ref: $ref\$|    ref: $resolved|" "$MANIFEST"
    echo "$name: $ref -> $resolved (pin updated)"
  else
    echo "$name: at $resolved"
  fi
done < <(parse)

exit $rc
