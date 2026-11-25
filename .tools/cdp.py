# 无头 Chrome 的 DevTools 客户端：ui_shot.py（模组卡片）与 ui_shots.py（全站逐页）共用一份。
#
# 为什么要抽出来：这两个脚本都要干同一件事——拉一个隔离 profile 的无头实例、
# 按 CSS 像素覆盖视口、等页面出现某个 DOM、点一个元素、截图。
# 各写一遍的话，"只杀自己拉的 chrome 进程"这条安全约束就会有两处实现，
# 迟早有一处写成 taskkill /IM chrome.exe 把用户自己的浏览器关掉。
#
# 视口一律走 Emulation.setDeviceMetricsOverride：不靠窗口大小，
# 这样 390 就是货真价实的 390 个 CSS 像素，手机上会不会横向溢出才量得准。
import base64
import json
import os
import socket
import struct
import subprocess
import time
import urllib.request

PORT = 9334
# 默认打本机开发面板；CDP_BASE 指到公网地址就能在同一套脚本上验云机那份成品
# （换装之后前端资源只有云机上那份是新的，只验本机等于没验）。
BASE = os.environ.get("CDP_BASE", "http://127.0.0.1:8963")
PROFILE = os.path.join(os.environ.get("TEMP", "."), "dstpanel-ui-shot-profile")
CHROME = r"C:\Program Files\Google\Chrome\Application\chrome.exe"
EDGE = r"C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe"
BIN = CHROME if os.path.exists(CHROME) else EDGE

# 只按 profile 路径匹配，别的 chrome 进程一个都不碰。
KILL_LEFTOVERS = ("Get-CimInstance Win32_Process -Filter \"Name='chrome.exe'\" | "
                  "ForEach-Object { if ($_.CommandLine -like '*dstpanel-ui-shot-profile*') { "
                  "Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue } }")


def http_json(path):
    with urllib.request.urlopen("http://127.0.0.1:%d%s" % (PORT, path), timeout=3) as r:
        return json.loads(r.read().decode("utf-8"))


def ws_connect(url):
    rest = url.split("//", 1)[1]
    hostport, _, path = rest.partition("/")
    host, _, port = hostport.partition(":")
    s = socket.create_connection((host, int(port or 80)), timeout=20)
    key = base64.b64encode(os.urandom(16)).decode()
    s.sendall(("GET /%s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
               "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n" % (path, hostport, key)).encode())
    buf = b""
    while b"\r\n\r\n" not in buf:
        buf += s.recv(4096)
    if b"101" not in buf.split(b"\r\n")[0]:
        raise RuntimeError("ws handshake failed: %r" % buf[:120])
    return s


class WS:
    """够用的 CDP WebSocket 客户端：只发文本帧，只收文本帧。"""

    def __init__(self, sock):
        self.s = sock
        self.buf = b""
        self.next_id = 0

    def _frame(self, payload):
        n = len(payload)
        mask = os.urandom(4)
        if n < 126:
            head = b"\x81" + bytes([0x80 | n])
        elif n < 65536:
            head = b"\x81" + bytes([0x80 | 126]) + struct.pack(">H", n)
        else:
            head = b"\x81" + bytes([0x80 | 127]) + struct.pack(">Q", n)
        self.s.sendall(head + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(payload)))

    def call(self, method, params=None, timeout=60):
        self.next_id += 1
        want = self.next_id
        self._frame(json.dumps({"id": want, "method": method, "params": params or {}}).encode())
        end = time.time() + timeout
        while time.time() < end:
            m = self._recv()
            if m and m.get("id") == want:
                if "error" in m:
                    raise RuntimeError("%s -> %s" % (method, m["error"]))
                return m.get("result")
        raise RuntimeError("timeout waiting for %s" % method)

    def _need(self, n):
        while len(self.buf) < n:
            got = self.s.recv(65536)
            if not got:
                raise RuntimeError("ws closed")
            self.buf += got

    def _recv(self):
        while True:
            self._need(2)
            b0, b1 = self.buf[0], self.buf[1]
            ln, off = b1 & 0x7F, 2
            if ln == 126:
                self._need(4)
                ln, off = struct.unpack(">H", self.buf[2:4])[0], 4
            elif ln == 127:
                self._need(10)
                ln, off = struct.unpack(">Q", self.buf[2:10])[0], 10
            self._need(off + ln)
            payload = bytes(self.buf[off:off + ln])
            self.buf = self.buf[off + ln:]
            op = b0 & 0x0F
            if op == 0x8:
                return None
            if op == 0x9:          # ping -> pong
                self.s.sendall(b"\x8a\x80" + os.urandom(4))
                continue
            if op == 0x1:
                return json.loads(payload.decode("utf-8"))


def ev(w, expression):
    r = w.call("Runtime.evaluate", {"expression": expression, "awaitPromise": True, "returnByValue": True})
    if r.get("exceptionDetails"):
        raise RuntimeError("page error: " + json.dumps(r["exceptionDetails"], ensure_ascii=False)[:400])
    return (r.get("result") or {}).get("value")


def until(w, expression, tries=60, pause=0.3):
    for _ in range(tries):
        if ev(w, expression):
            return True
        time.sleep(pause)
    return False


def click(w, selector, predicate):
    return ev(w, "(() => { const b = [...document.querySelectorAll(%s)].find(%s);"
                 " b && b.click(); return !!b; })()" % (selector, predicate))


def shoot(w, png):
    d = os.path.dirname(png)
    if d:
        os.makedirs(d, exist_ok=True)
    with open(png, "wb") as f:
        f.write(base64.b64decode(w.call("Page.captureScreenshot", {"format": "png"})["data"]))


def hover(b, x, y):
    """把系统级鼠标移到页面坐标上。

    悬停这类链路只认真事件：在页面里合成 new PointerEvent('mouseenter') 派发下去，
    读数条、说明气泡一类都不会更新（实测踩过）。另外 focus() 这条路也别用来开气泡——
    页面（文档）还没拿到焦点时 Chrome 只改 activeElement、不发 focus 事件，
    云机那一次就是这么量到"按钮在、气泡永远不出来"的。
    """
    b.ws.call("Input.dispatchMouseEvent",
              {"type": "mouseMoved", "x": x, "y": y, "button": "none"})


class Browser:
    """开一个页面、按视口量布局。实例自己负责拉 Chrome，退出时只关自己拉的那个。"""

    def __init__(self, width=390, height=844):
        self.child = None
        try:
            http_json("/json/version")
        except Exception:
            subprocess.run(["powershell.exe", "-NoProfile", "-Command", KILL_LEFTOVERS], capture_output=True)
            subprocess.run(["cmd", "/c", "rmdir", "/s", "/q", PROFILE], capture_output=True)
            self.child = subprocess.Popen(
                [BIN, "--headless=new", "--disable-gpu", "--no-first-run", "--hide-scrollbars",
                 # 机器上挂着系统代理时，Chrome 会把 127.0.0.1:8963 也送进代理，
                 # 回一张 502，页面根本没有 #app（实测踩过）。本地面板一律直连。
                 "--no-proxy-server",
                 "--remote-debugging-port=%d" % PORT, "--user-data-dir=%s" % PROFILE, "about:blank"],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            for _ in range(80):
                try:
                    http_json("/json/version")
                    break
                except Exception:
                    time.sleep(0.25)
            else:
                raise SystemExit("无头 Chrome 起不来：端口 %d 上没有 DevTools" % PORT)
        target = None
        for _ in range(40):
            pages = [t for t in http_json("/json/list") if t.get("type") == "page" and t.get("webSocketDebuggerUrl")]
            if pages:
                target = pages[0]
                break
            time.sleep(0.25)
        if not target:
            raise SystemExit("没有 page target")
        self.ws = WS(ws_connect(target["webSocketDebuggerUrl"]))
        self.ws.call("Page.enable")
        self.ws.call("Runtime.enable")
        # set_files 要靠 DOM 域把 objectId 换成节点 id；不 enable 时 requestNode 直接报错。
        self.ws.call("DOM.enable")
        self.set_viewport(width, height)

    def set_viewport(self, width, height):
        self.width, self.height = width, height
        self.ws.call("Emulation.setDeviceMetricsOverride",
                     {"width": width, "height": height, "deviceScaleFactor": 1, "mobile": width <= 560})

    def goto(self, url):
        self.ws.call("Page.navigate", {"url": url})

    def reload(self):
        # 同一个 URL 再 navigate 一次浏览器不会重载，必须显式 reload。
        # 但 Chrome 刚拉起来那一格是 about:blank：从它跨到 http 源以后 Page.reload 会报
        # "Not attached to an active page"（ui_cold_start 里改成带参数的 goto 才绕开，
        # 验收轮里又随机红在登录那一步，和被测的页面没关系）。所以这里兜一次：
        # 换成"带一个每次都变的查询参数"的 goto，效果等同整页刷新——面板只认 hash。
        try:
            self.ws.call("Page.reload")
            return
        except RuntimeError as e:
            href = ""
            try:
                href = self.js("location.href") or ""
            except Exception:
                raise e
            if not href.startswith("http"):
                raise e
            base, sep, frag = href.partition("#")
            self.goto("%s?cdp=%d%s%s" % (base, int(time.time() * 1000), sep, frag))

    def wait(self, expression, tries=60, pause=0.3):
        return until(self.ws, expression, tries, pause)

    def js(self, expression):
        return ev(self.ws, expression)

    def click(self, selector, predicate):
        return click(self.ws, selector, predicate)

    def shoot(self, png):
        return shoot(self.ws, png)

    def set_files(self, selector, paths):
        """往 <input type=file> 里塞真实盘上的文件，等价于用户点"选文件"。

        为什么不能用 JS 造 File 代替：导入那条动线要把包真的传到接口，
        合成文件只能验到 dataTransfer 那一段，验不出 multipart 字段名、
        上限拦人、接口回的结构这三样。这里走 DOM.setFileInputFiles，
        浏览器会自己派发 change 事件，Vue 那侧和真人操作看到的是同一个事件。

        节点 id 每次重新取：DOM.requestNode 拿到的 id 会在下一次导航/重绘后失效
        （实测报 "Could not find node with given id"），所以这里按 getDocument →
        querySelector 走一遍，不复用任何缓存。
        """
        try:
            doc = self.ws.call("DOM.getDocument", {"depth": 1})
            root = doc["root"]["nodeId"]
            node = self.ws.call("DOM.querySelector", {"nodeId": root, "selector": selector})
            if not node.get("nodeId"):
                return False
            self.ws.call("DOM.setFileInputFiles", {"files": paths, "nodeId": node["nodeId"]})
            return True
        except RuntimeError:
            return False

    def close(self):
        if self.child is not None:
            try:
                self.ws.call("Browser.close")
            except Exception:
                pass
            try:
                self.child.wait(timeout=10)
            except Exception:
                self.child.kill()
        else:
            self.ws.s.close()
