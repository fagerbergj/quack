#!/usr/bin/env bash
# Re-sync the vendored skill-library plugins in .agents/vendor from upstream.
#
# The vendored trees are the build input, not a cache - this script is only
# how they get updated. It refuses to overwrite a tree that has local edits on
# top of its recorded ref (FORCE=1 to override), because those edits exist
# nowhere else: they are not committed upstream.
#
#   scripts/sync-plugins.sh              # every plugin, to upstream HEAD
#   scripts/sync-plugins.sh ponytail     # one plugin
#   REF=v4.7.0 scripts/sync-plugins.sh ponytail
#   FORCE=1 scripts/sync-plugins.sh      # discard local edits
set -euo pipefail

cd "$(dirname "$0")/.."
MANIFEST=.agents/vendor/plugins.yaml
[[ -f $MANIFEST ]] || { echo "no $MANIFEST" >&2; exit 1; }

# Fixed-shape parse of MANIFEST into name<TAB>url<TAB>ref<TAB>path rows.
parse() {
  awk '
    /^  - name:/ { if (n != "") print n "\t" u "\t" r "\t" p; n=$3; u=r=p="" ; next }
    /^    url:/  { u=$2; next }
    /^    ref:/  { r=$2; next }
    /^    path:/ { p=$2; next }
    END { if (n != "") print n "\t" u "\t" r "\t" p }
  ' "$MANIFEST"
}

want=${1:-}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
rc=0

while IFS=$'\t' read -r name url ref path; do
  [[ -n $want && $want != "$name" ]] && continue
  [[ -n $name && -n $url && -n $ref && -n $path ]] || { echo "manifest entry incomplete: $name" >&2; rc=1; continue; }

  echo "==> $name ($path)"
  git clone --quiet "$url" "$tmp/$name"

  # Local edits = vendored tree differs from the ref it was synced from.
  if git -C "$tmp/$name" checkout --quiet "$ref" 2>/dev/null; then
    if ! diff -rq --exclude=.git "$tmp/$name" "$path" >/dev/null 2>&1; then
      if [[ ${FORCE:-} != 1 ]]; then
        echo "    local edits on top of $ref - refusing to overwrite. Push them upstream, or FORCE=1 to discard:" >&2
        diff -rq --exclude=.git "$tmp/$name" "$path" 2>&1 | sed 's/^/      /' >&2
        rc=1
        rm -rf "${tmp:?}/$name"
        continue
      fi
      echo "    FORCE=1: discarding local edits"
    fi
  else
    echo "    recorded ref $ref is not in upstream; cannot check for local edits" >&2
    [[ ${FORCE:-} == 1 ]] || { rc=1; rm -rf "${tmp:?}/$name"; continue; }
  fi

  target=${REF:-origin/HEAD}
  git -C "$tmp/$name" checkout --quiet "$target" 2>/dev/null || git -C "$tmp/$name" checkout --quiet "${target#origin/}"
  new=$(git -C "$tmp/$name" rev-parse HEAD)

  rm -rf "${tmp:?}/$name/.git"
  rm -rf "${path:?}"
  mkdir -p "$path"
  cp -a "$tmp/$name/." "$path/"

  if [[ $new == "$ref" ]]; then
    echo "    already at $new"
  else
    # Refs are 40-hex and unique per entry, so an anchored line match is enough.
    sed -i "s|^    ref: $ref\$|    ref: $new|" "$MANIFEST"
    echo "    $ref -> $new"
  fi
  rm -rf "${tmp:?}/$name"
done < <(parse)

exit $rc
