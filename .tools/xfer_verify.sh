#!/bin/bash
# 核对云端服务端包是否与本地源逐文件一致。
# 为什么要清单而不是只看 du/find 计数：两对 tar 曾同时往同一目录解包，
# 计数对上不代表每个文件的大小都对，必须按"路径+字节数"逐项比。
#
# 上一版用 comm 直接失败了：本地是 Git Bash 的 sort、云端是 Linux 的 sort，
# 两边 collation 不一样，comm 报 "not in sorted order" 然后给出 2028/2223 这种废数。
# 现在统一加 LC_ALL=C，并且用 python 按字典比，把"缺失"和"大小不同"分开。
set +e
SRC=/d/dst-transfer/server_linux
DEST_DIR=/home/ubuntu/dst/dedicated_server
# 清单落在纯 ASCII 路径：中文路径经过去 Windows python 的 argv/stdin 会被转码成乱码，
# 报出来的 FileNotFoundError 里那串问号就是这么来的。
L=/d/dst-transfer/.verify/manifest_local.tsv
R=/d/dst-transfer/.verify/manifest_remote.tsv
mkdir -p /d/dst-transfer/.verify

echo "=== 生成清单 $(date +%H:%M:%S) ==="
(cd "$SRC" && find . -printf '%y\t%p\t%s\n' | LC_ALL=C sort > "$L")
ssh dst-cloud 'cd "'"$DEST_DIR"'" && find . -printf "%y\t%p\t%s\n" | LC_ALL=C sort > /tmp/manifest_remote.txt; du -sh "'"$DEST_DIR"'"'
scp -q dst-cloud:/tmp/manifest_remote.txt "$R"
echo "本地条目: $(wc -l < "$L")   云端条目: $(wc -l < "$R")"

python - "$L" "$R" <<'PY'
import io, sys

def winish(p):
    # Windows python 不认识 /d/... 这种 MSYS 路径，换成 D:/... 再说。
    import subprocess
    return subprocess.check_output(['cygpath', '-w', p], text=True).strip().replace('\\', '/')

def load(path):
    size, kind = {}, {}
    for line in io.open(path, encoding='utf-8', errors='replace'):
        parts = line.rstrip('\n').split('\t')
        if len(parts) != 3:
            continue
        k, p, s = parts
        size[p] = int(s)
        kind[p] = k
    return size, kind

local, lkind = load(winish(sys.argv[1]))
remote, rkind = load(winish(sys.argv[2]))

# 目录的 st_size 是目录项块数，两边文件系统不同就一定对不上，比它只会制造噪音。
lfiles = set(p for p in local if lkind[p] == 'f')
rfiles = set(p for p in remote if rkind[p] == 'f')
ldirs = set(p for p in local if lkind[p] == 'd')
rdirs = set(p for p in remote if rkind[p] == 'd')

missing_files = sorted(lfiles - rfiles)
missing_dirs = sorted(ldirs - rdirs)
differs = sorted(p for p in lfiles & rfiles if local[p] != remote[p])
extra = sorted(p for p in (set(remote) - set(local)) if '.tmp-' not in p)
extras = sum(remote[p] for p in extra if p in rfiles)
top = sorted(set(p.split('/')[1] if '/' in p.strip('/') else p for p in extra))

print('文件缺失: %d   目录缺失: %d   文件大小不符: %d' % (len(missing_files), len(missing_dirs), len(differs)))
print('云端多余: %d 项，合计 %.1f MB' % (len(extra), extras / 1048576.0))
print('  多余项分布在: %s' % '、'.join(x for x in top if x)[:200])
for name, items in [('文件缺失', missing_files), ('目录缺失', missing_dirs), ('大小不符', differs)]:
    for p in items[:15]:
        print('  [%s] %s' % (name, p))
    if len(items) > 15:
        print('  [%s] ... 还有 %d 条' % (name, len(items) - 15))
PY

echo "=== 关键文件 ==="
ssh dst-cloud 'D="'"$DEST_DIR"'";
 B="$D"/bin64/dontstarve_dedicated_server_nullrenderer_x64;
 ls -la "$B";
 file "$B" | cut -c1-120;
 OUT=$(ldd "$B" 2>&1);
 if [ -z "$OUT" ]; then echo "  ldd 没有输出，这个检查没做成"; else
   BAD=$(echo "$OUT" | grep -i "not found");
   if [ -z "$BAD" ]; then echo "  依赖完整"; else echo "  缺依赖:"; echo "$BAD"; fi
 fi;
 ls "$D"/bin64 | head -20'
