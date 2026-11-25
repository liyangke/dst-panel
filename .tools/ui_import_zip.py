# -*- coding: utf-8 -*-
"""造"别人给的存档包"，用来在浏览器里实测导入这条动线。

测试里的包是在 Go 单测的临时目录里现造的，界面实测没法用那份——浏览器只能选盘上真实
存在的文件。所以这一份把同样几种形状写到 .tmp_boot/import/ 下：

  friend-cluster.zip    整间房间（最外层是房间目录），能导
  friend-shard.zip      只有一个世界目录（shard 形状），也能导
  notes-only.zip        合法压缩包、但不是饥荒存档：要出"不能导"的结论页
  screenshot.png        根本不是压缩包：接口要回 400，界面要报错不弹登录层
  oversize.bin          600MB 的稀疏文件：验证界面在上限就把人拦住，不发请求

oversize.bin 是 truncate 出来的稀疏文件，很快；真的传上去反而要几分钟，那正是要拦住的事。
"""
import io
import os
import sys
import zipfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, ".tmp_boot", "import")

# 16 位十六进制：引擎的世界编号（GAMEID）就是这个形状，导入全程不许改它。
GAMEID = "AB12CD34EF56AB77"


def shard_index(name):
    return (
        "KLEI     1 \n"
        "return {\n"
        '    session_id = "' + GAMEID + '",\n'
        '    name = "' + name + '",\n'
        '    root = "' + name + '",\n'
        "    enabled = true,\n"
        "}\n"
    )


def world_tree(pre, role="MASTER"):
    """一个世界目录：存档 + 那个世界的配置，另外掺进几样必须被丢掉的脏东西。

    pre 形如 "Cluster_77/Master" 或单独的 "Master"，返回的键全以 pre 打头，
    房间级文件（cluster.ini / cluster_token.txt）由调用方另加，别混进来。
    """
    sess = pre + "/save/session/" + GAMEID + "/"
    shard = pre + "/save/"
    return {
        shard + "shardindex": shard_index(pre.rsplit("/", 1)[-1]),
        sess + "0000000085": ("地表世界存档 " * 400) + "\n",
        sess + "0000000085.meta": "return {clock={cycles=174}}\n",
        sess + "slot-0001/server.dat": "客户端那份存档 " * 30 + "\n",
        shard + "dstpanel_queue.txt": "cmd-1|kick|阿黎|恶意发言\n",
        pre + "/server_log.txt": "[00:00:01]: 上次开机的日志\n",
        pre + "/server_chat_log.txt": "说过的话\n",
        pre + "/server.ini": "[SHARD]\nrole=" + role + "\nname=" + pre.rsplit("/", 1)[-1] + "\n",
        pre + "/modoverrides.lua": "return { [\"workshop-378160973\"] = { configuration_options = {} } }\n",
        pre + "/leveldataoverride.lua": "return { ground_settings = {}, override_enabled = true }\n",
    }


def cluster_tree(pre):
    return {
        pre + "/cluster.ini": "cluster_name = 朋友给的房间\nmax_players = 6\n",
        pre + "/cluster_token.txt": "KLEI-FAKE-TOKEN-DO-NOT-PRINT\n",
    }


def write_zip(path, files):
    names = sorted(files.keys())
    with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED) as zf:
        for n in names:
            zf.writestr(n, files[n])
    return path, os.path.getsize(path)


def main():
    if not os.path.isdir(OUT):
        os.makedirs(OUT)

    made = []

    files = {}
    files.update(cluster_tree("Cluster_77"))
    files.update(world_tree("Cluster_77/Master", "MASTER"))
    files.update(world_tree("Cluster_77/Caves", "CAVES"))
    # 这两样是打包时夹带的面板配置，结构上不属于世界目录，应该出现在"没取的东西"里。
    files["readme.txt"] = "这个包是 2026 年 10 月那次存档，用 7-Zip 打的。\n"
    files["__MACOSX/._cluster.ini"] = "苹果压缩带出来的垃圾\n"
    made.append(write_zip(os.path.join(OUT, "friend-cluster.zip"), files))

    shard_only = world_tree("Master", "MASTER")
    made.append(write_zip(os.path.join(OUT, "friend-shard.zip"), shard_only))

    made.append(write_zip(os.path.join(OUT, "notes-only.zip"), {
        "notes/plan.txt": "下次想建个森林房\n",
        "notes/screenshots.txt": "截图在这里，但没有存档文件\n",
    }))

    png = os.path.join(OUT, "screenshot.png")
    with open(png, "wb") as fh:
        fh.write(b"\x89PNG\r\n\x1a\n" + ("这不是压缩包，只是一张图片的内容 " * 20).encode("utf-8"))
    made.append((png, os.path.getsize(png)))

    big = os.path.join(OUT, "oversize.bin")
    with io.open(big, "wb") as fh:
        fh.truncate(600 * 1024 * 1024)
    made.append((big, os.path.getsize(big)))

    for p, n in made:
        print("%s  %d 字节" % (p, n))


if __name__ == "__main__":
    sys.exit(main())
