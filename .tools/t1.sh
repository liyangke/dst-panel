#!/bin/bash
# 侦查第二步：符号名清单能不能对上我们的 prefab；顺便看清 tex 头和有没有 Pillow。
D=/home/ubuntu/dst/dedicated_server/data/minimap
echo "=== 四个图集的符号总数 ==="
for f in "$D"/minimap_data.xml "$D"/minimap_data1.xml "$D"/minimap_data2.xml "$D"/minimap_data3.xml; do
  printf "%-24s %s\n" "$(basename "$f")" "$(grep -o 'name="[^"]*"' "$f" | wc -l)"
done
echo "=== 我们要的这些 prefab 有没有现成符号 ==="
cat "$D"/minimap_data*.xml | grep -o 'name="[^"]*"' | sed 's/name="//; s/\.png"//; s/"//' | sort -u > /tmp/syms.txt
wc -l < /tmp/syms.txt
for p in berrybush berrybush_planted evergreen deciduoustree grass sapling reeds cactus \
         flower flower_evil mushroom_light mushroom_red mushroom_blue mushroom_brown \
         mushroom_spoor glowcap rock1 rock2 rock3 boulder_1 boulder_2 boulder_3 marble_1 \
         spiderden houndmound killerbeehive tentacle walrus_camp chester glommer \
         campfire sciencemachine chest wall_hay bownut farmplot pighead trap \
         beefalo rabbit deer spider bee pig tallbird cave_banana bush; do
  hit=$(grep -ix "$p" /tmp/syms.txt | head -1)
  printf "%-18s %s\n" "$p" "${hit:-（没有）}"
done
echo "=== 名字样本：含 berry / rock / evergreen 的 ==="
grep -i "berry" /tmp/syms.txt | head -6
grep -i "rock\|boulder" /tmp/syms.txt | head -6
grep -i "evergreen\|deciduo\|tree" /tmp/syms.txt | head -8
echo "=== tex 头 32 字节 ==="
for f in "$D"/minimap_atlas.tex "$D"/minimap_atlas1.tex "$D"/minimap_atlas3.tex; do
  printf "%-22s " "$(basename "$f")"; xxd -p -l 32 "$f"
done
echo "=== Pillow / numpy ==="
python3 -c "import PIL, sys; print('PIL', PIL.__version__)" 2>&1 | head -2
python3 -c "import numpy; print('numpy', numpy.__version__)" 2>&1 | head -2
