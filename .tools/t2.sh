#!/bin/bash
cd /tmp/sc
echo "=== chester / glommer 的 tag ==="
for f in scripts/prefabs/chester.lua scripts/prefabs/glommer.lua scripts/prefabs/pigfoot.lua; do
  [ -f "$f" ] && { echo "--- $f"; grep -n "AddTag\|inst.tags\|MakePlayerAttacher\|companion" "$f" | head -6; }
done
echo "=== 引擎里 companion 这个 tag 谁在用 ==="
grep -rl '"companion"' scripts/prefabs/ | head -12
echo "=== GROUND_NAMES 是运行期填的，看有没有现成的地表名可用 ==="
grep -n "GROUND_NAMES\[" -A 3 scripts/constants.lua | head -12
