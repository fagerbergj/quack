#!/usr/bin/env bash
# Exercise sync-plugins.sh's UPDATE path against a local throwaway upstream:
# clean tree -> syncs and rewrites the ref; dirty tree -> refuses; FORCE -> overwrites.
set -euo pipefail
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t

# upstream repo with two commits
up=$T/upstream; mkdir -p "$up/skills/demo"
git -C "$up" init -q -b main
printf 'v1\n' > "$up/skills/demo/SKILL.md"
printf '{"name":"demo"}\n' > "$up/plugin.json"
git -C "$up" add -A && git -C "$up" commit -qm one
old=$(git -C "$up" rev-parse HEAD)
printf 'v2\n' > "$up/skills/demo/SKILL.md"
git -C "$up" add -A && git -C "$up" commit -qm two
new=$(git -C "$up" rev-parse HEAD)

# fake repo root: script + manifest + vendored tree at `old`
root=$T/repo; mkdir -p "$root/scripts" "$root/.agents/vendor"
cp "$(cd "$(dirname "$0")" && pwd)/sync-plugins.sh" "$root/scripts/"
git clone -q "$up" "$root/.agents/vendor/demo"
git -C "$root/.agents/vendor/demo" checkout -q "$old"
rm -rf "$root/.agents/vendor/demo/.git"
cat > "$root/.agents/vendor/plugins.yaml" <<EOF
plugins:
  - name: demo
    url: $up
    ref: $old
    path: .agents/vendor/demo
EOF

echo "--- case 1: clean tree, should sync old -> new ---"
(cd "$root" && ./scripts/sync-plugins.sh)
grep -q "ref: $new" "$root/.agents/vendor/plugins.yaml" && echo "PASS manifest ref updated" || { echo "FAIL manifest ref"; exit 1; }
grep -q v2 "$root/.agents/vendor/demo/skills/demo/SKILL.md" && echo "PASS files updated" || { echo "FAIL files"; exit 1; }
[[ ! -e $root/.agents/vendor/demo/.git ]] && echo "PASS no nested .git" || { echo "FAIL nested .git"; exit 1; }

echo "--- case 2: dirty tree, should refuse and change nothing ---"
printf 'local\n' >> "$root/.agents/vendor/demo/skills/demo/SKILL.md"
before=$(cat "$root/.agents/vendor/demo/skills/demo/SKILL.md")
set +e; (cd "$root" && ./scripts/sync-plugins.sh >/dev/null 2>&1); rc=$?; set -e
[[ $rc -ne 0 ]] && echo "PASS refused (exit $rc)" || { echo "FAIL should have refused"; exit 1; }
[[ "$(cat "$root/.agents/vendor/demo/skills/demo/SKILL.md")" == "$before" ]] && echo "PASS tree untouched" || { echo "FAIL tree modified"; exit 1; }

echo "--- case 3: FORCE=1, should discard local edits ---"
(cd "$root" && FORCE=1 ./scripts/sync-plugins.sh >/dev/null)
grep -qx v2 "$root/.agents/vendor/demo/skills/demo/SKILL.md" && echo "PASS local edits discarded" || { echo "FAIL"; exit 1; }
echo "ALL PASS"
