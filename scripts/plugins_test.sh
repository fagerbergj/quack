#!/usr/bin/env bash
# Drives scripts/plugins.sh against a throwaway local upstream (no network).
set -euo pipefail
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t

up=$T/upstream; mkdir -p "$up/skills/demo"
git -C "$up" init -q -b main
printf 'v1\n' > "$up/skills/demo/SKILL.md"
printf '{"name":"demo"}\n' > "$up/plugin.json"
git -C "$up" add -A && git -C "$up" commit -qm one
old=$(git -C "$up" rev-parse HEAD)
printf 'v2\n' > "$up/skills/demo/SKILL.md"
git -C "$up" add -A && git -C "$up" commit -qm two
new=$(git -C "$up" rev-parse HEAD)

root=$T/repo; mkdir -p "$root/scripts" "$root/.agents/vendor"
cp "$(cd "$(dirname "$0")" && pwd)/plugins.sh" "$root/scripts/"
manifest=$root/.agents/vendor/plugins.yaml
cat > "$manifest" <<EOF
plugins:
  - name: demo
    url: $up
    ref: $old
    path: .agents/vendor/demo
EOF
tree=$root/.agents/vendor/demo

echo "--- fetch to the pinned ref ---"
(cd "$root" && ./scripts/plugins.sh >/dev/null)
grep -qx v1 "$tree/skills/demo/SKILL.md" && echo "PASS fetched pinned content" || { echo "FAIL content"; exit 1; }
[[ $(cat "$tree/.plugin-ref") == "$old" ]] && echo "PASS stamped ref" || { echo "FAIL stamp"; exit 1; }
[[ ! -e $tree/.git ]] && echo "PASS no nested .git" || { echo "FAIL nested .git"; exit 1; }

echo "--- re-run is idempotent and offline (upstream moved away) ---"
mv "$up" "$T/upstream-hidden"
out=$(cd "$root" && ./scripts/plugins.sh)
grep -q "already at $old" <<<"$out" && echo "PASS skipped without network" || { echo "FAIL not idempotent: $out"; exit 1; }
mv "$T/upstream-hidden" "$up"

echo "--- --update moves the pin to HEAD ---"
(cd "$root" && ./scripts/plugins.sh --update >/dev/null)
grep -qx v2 "$tree/skills/demo/SKILL.md" && echo "PASS updated content" || { echo "FAIL update"; exit 1; }
grep -q "ref: $new" "$manifest" && echo "PASS manifest pin moved" || { echo "FAIL manifest"; exit 1; }

echo "--- unreachable upstream keeps the tree on disk ---"
sed -i "s|url: .*|url: $T/does-not-exist|" "$manifest"
rm -f "$tree/.plugin-ref"   # force it to try
set +e; (cd "$root" && ./scripts/plugins.sh >/dev/null 2>&1); rc=$?; set -e
grep -qx v2 "$tree/skills/demo/SKILL.md" && echo "PASS tree preserved offline" || { echo "FAIL tree lost"; exit 1; }
[[ $rc -eq 0 ]] && echo "PASS offline-with-tree is not an error" || { echo "FAIL rc=$rc"; exit 1; }

echo "--- unreachable upstream with NO tree fails ---"
rm -rf "$tree"
set +e; (cd "$root" && ./scripts/plugins.sh >/dev/null 2>&1); rc=$?; set -e
[[ $rc -ne 0 ]] && echo "PASS hard fail with nothing on disk" || { echo "FAIL should have failed"; exit 1; }

echo "--- a pin that does not exist upstream fails ---"
sed -i "s|url: .*|url: $up|" "$manifest"
sed -i "s|ref: .*|ref: 0000000000000000000000000000000000000000|" "$manifest"
set +e; (cd "$root" && ./scripts/plugins.sh >/dev/null 2>&1); rc=$?; set -e
[[ $rc -ne 0 ]] && echo "PASS bad pin rejected" || { echo "FAIL bad pin accepted"; exit 1; }

echo "ALL PASS"
