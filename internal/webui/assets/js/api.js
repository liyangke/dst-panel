// 接口层：所有对 /api/v1/ 的访问都走这里。
//
// 认证是 HTTP Basic（面板默认只绑 127.0.0.1，服务端不存会话），所以：
//   - 凭据只留在内存里，配合可选的 sessionStorage（关标签页即失效，不落盘）；
//   - 401 一律抛给界面弹登录层，而不是让每个页面各自处理"没权限"；
//   - 每个响应都归一成 {ok, status, data, error}：接口的错误信息本来就是中文结论，
//     界面原样显示即可，不做任何二次加工，免得把"要先停掉分片"改成别的话。
let creds = null;
let onUnauthorized = null;

const KEY = "dstpanel.creds";

export function setCredentials(user, pass, remember) {
  creds = { user, pass };
  if (remember) sessionStorage.setItem(KEY, JSON.stringify(creds));
  else sessionStorage.removeItem(KEY);
}

export function hasStoredCredentials() {
  const raw = sessionStorage.getItem(KEY);
  if (!raw) return false;
  try {
    const c = JSON.parse(raw);
    creds = { user: c.user, pass: c.pass };
    return !!c.user;
  } catch (e) {
    sessionStorage.removeItem(KEY);
    return false;
  }
}

export function clearCredentials() {
  creds = null;
  sessionStorage.removeItem(KEY);
}

export function onUnauthorizedSet(fn) { onUnauthorized = fn; }

// b64 必须按 UTF-8 编：btoa 只认 Latin1，管理员密码里有一个中文就会在这里抛异常，
// 而那个异常发生在"点登录"的回调里，界面只会表现为按了没反应。
function basicHeader(c) {
  const bytes = new TextEncoder().encode(c.user + ":" + c.pass);
  let bin = "";
  bytes.forEach((b) => { bin += String.fromCharCode(b); });
  return "Basic " + btoa(bin);
}

function headers(extra) {
  const h = Object.assign({ Accept: "application/json" }, extra || {});
  if (creds) h.Authorization = basicHeader(creds);
  return h;
}

// request 发一次接口调用。body 传对象时按 JSON 发；传 FormData 时交给浏览器拼 multipart。
export async function request(method, path, body) {
  const init = { method, headers: headers(), credentials: "omit" };
  if (body !== undefined && body !== null) {
    if (body instanceof FormData) {
      init.body = body;            // 不能自己设 Content-Type，boundary 要浏览器填
    } else {
      init.headers["Content-Type"] = "application/json";
      init.body = JSON.stringify(body);
    }
  }
  let res;
  try {
    res = await fetch(path, init);
  } catch (e) {
    // 面板挂了 / 网络断了：这一条不弹登录层，因为账号是对的，说了反而误导。
    return { ok: false, status: 0, data: null,
      error: "连不上面板接口（" + method + " " + path + "）。面板进程可能已退出或网络不通：" + e.message };
  }
  const ct = res.headers.get("content-type") || "";
  let data = null, errText = "";
  if (ct.indexOf("application/json") >= 0) {
    // 不能用 res.json().catch(()=>({}))：解析失败被吞成 {} 之后，下面会走到
    // return {ok:true,data:null}，列表页就渲染出一个"真的没有数据"的假空态。
    // 用户看到的是空列表，实际是面板版本不匹配、反代截断或返回了半截 HTML。
    const raw = await res.text().catch(() => "");
    let j = null;
    try { j = raw ? JSON.parse(raw) : {}; } catch (e) {
      return { ok: false, status: res.status, data: null,
        error: "接口返回的 JSON 读不懂（" + method + " " + path + "）：" + e.message +
          "。可能是面板刚升级、响应被代理截断，或这个地址不是接口；刷新后再试，别把空列表当成真的没数据。" };
    }
    data = j.data === undefined ? null : j.data;
    errText = j.error || "";
  } else {
    const text = await res.text().catch(() => "");
    // 拿到 HTML 说明请求根本没被接口层接住（多半是 URL 拼错后被前端路由兜底了）。
    // 这里必须报出来，不能当成"数据是空"，那会让页面显示一个假的空列表。
    if (res.ok) {
      return { ok: false, status: res.status, data: null,
        error: "接口回的不是 JSON，拿到的是 " + (ct || "未知类型") + "；这个地址可能没注册" };
    }
    errText = text.slice(0, 300);
  }
  if (res.status === 401) {
    // 不清凭据、不重渲染成空白：用户可能正填着开服向导或一张长表单，
    // 401 那一刻把整页换掉、丢掉已填内容，是最难受的一种"静默降级"。
    // 由 store 侧停掉轮询、显示「会话过期」并把登录层浮在现有内容之上；
    // 用户重新登录后回到原处，表单还在。
    if (onUnauthorized) onUnauthorized();
    return { ok: false, status: 401, data: null,
      error: errText || "登录状态已失效或账号密码不对，请重新登录" };
  }
  if (!res.ok) {
    return { ok: false, status: res.status, data: null, error: errText || ("接口返回 " + res.status) };
  }
  return { ok: true, status: res.status, data, error: "" };
}

export const get = (p) => request("GET", p);
export const post = (p, b) => request("POST", p, b === undefined ? {} : b);
export const put = (p, b) => request("PUT", p, b);
export const del = (p) => request("DELETE", p);

// download 用 fetch 拿二进制再交给 <a download>：直接开新标签页会走浏览器自己的
// Basic 认证弹窗，用户会在那里再输一次密码，而且失败时只会看到一个空页面。
export async function download(path, filename) {
  let res;
  try {
    res = await fetch(path, { headers: headers(), credentials: "omit" });
  } catch (e) {
    return { ok: false, error: "下载没发出去：" + e.message };
  }
  if (!res.ok) {
    let msg = "接口返回 " + res.status;
    if ((res.headers.get("content-type") || "").indexOf("application/json") >= 0) {
      const j = await res.json().catch(() => ({}));
      msg = j.error || msg;
    }
    if (res.status === 401 && onUnauthorized) onUnauthorized();
    return { ok: false, error: msg };
  }
  const blob = await res.blob();
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename || "";
  document.body.appendChild(a);
  a.click();
  a.remove();
  // 立刻 revoke 会让部分浏览器还没开始写文件就拿到空 URL，留一秒再由浏览器自己回收。
  setTimeout(() => URL.revokeObjectURL(url), 2000);
  return { ok: true, size: blob.size };
}

// qs 拼查询串。空值一律跳过：界面里"没选筛选项"和"选了空字符串"是同一件事，
// 但后端会把 cluster= 当成一个真实的空集群名去拒绝。
export function qs(pairs) {
  const out = [];
  Object.keys(pairs).forEach((k) => {
    const v = pairs[k];
    if (v === undefined || v === null || v === "") return;
    out.push(encodeURIComponent(k) + "=" + encodeURIComponent(String(v)));
  });
  return out.length ? "?" + out.join("&") : "";
}

// stream 订阅后端那条 SSE 日志流，用 fetch 而不是 EventSource。
//
// 为什么不用 EventSource：它不能带请求头，而面板的认证是 HTTP Basic，
// 于是只剩"把凭据塞进 URL 查询参数"这一条路——那玩意的密码会进浏览器历史、
// 进访问日志、也进面板自己的请求日志（logRequests 记的是完整 path）。
// fetch 能带头也能中止，代价是 SSE 的帧格式要自己解，就是下面这几十行。
//
// onFrame(event, data) 每条帧回调一次；后端的帧是 meta / line / done / reset / error，
// 冒号开头的注释帧（keepalive）直接丢掉。signal 由调用方给，切页面时 AbortController 一断就干净了。
export async function stream(path, onFrame, signal, extraHeaders) {
  let res;
  try {
    res = await fetch(path, { headers: headers(Object.assign({ Accept: "text/event-stream" }, extraHeaders || {})),
      credentials: "omit", signal });
  } catch (e) {
    if (e.name === "AbortError") return { ok: true, aborted: true };
    return { ok: false, error: "连不上日志流：" + e.message };
  }
  if (!res.ok) {
    // 头几帧还没写出去时后端仍是普通 JSON 错误（参数错、分片没启动、密码不对），
    // 这些必须当"订阅失败"处理，不能让界面显示一个空的日志框。
    let msg = "接口返回 " + res.status;
    if ((res.headers.get("content-type") || "").indexOf("application/json") >= 0) {
      const j = await res.json().catch(() => ({}));
      msg = j.error || msg;
    }
    if (res.status === 401) { if (onUnauthorized) onUnauthorized(); }
    return { ok: false, status: res.status, error: msg };
  }
  // 200 但 content-type 不是 event-stream：这个地址根本不是流式接口（/log 与 /log/stream 就是两个路由）。
  // 这一步不拦，那一整行 JSON 会被当成"帧还没到"，界面只剩一个既不报错也不出行的空日志框——
  // 实测踩过，报错比空屏有用得多。
  const ct = res.headers.get("content-type") || "";
  if (ct.indexOf("text/event-stream") < 0) {
    let msg = "这个地址没有返回日志流（content-type 是 " + (ct.split(";")[0] || "空") + "）";
    if (ct.indexOf("application/json") >= 0) {
      const j = await res.json().catch(() => ({}));
      if (j.error) msg = j.error;
    }
    return { ok: false, status: res.status, error: msg };
  }
  if (!res.body) return { ok: false, error: "这个浏览器不支持流式读取，日志只能按刷新看" };
  const reader = res.body.getReader();
  const dec = new TextDecoder("utf-8");
  let buf = "";
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      // SSE 的帧分隔是一个空行，也就是连续两个换行。
      // 一条帧的 data 里不含裸换行（后端写的是单行 JSON），所以按 "\n\n" 切是安全的。
      for (;;) {
        const i = buf.indexOf("\n\n");
        if (i < 0) break;
        const raw = buf.slice(0, i);
        buf = buf.slice(i + 2);
        dispatch(raw, onFrame);
      }
    }
  } catch (e) {
    if (e.name === "AbortError") return { ok: true, aborted: true };
    return { ok: false, error: "日志流读断了：" + e.message };
  }
  if (buf.trim()) dispatch(buf, onFrame);
  return { ok: true };
}

function dispatch(raw, onFrame) {
  let event = "message";
  let id = "";
  const data = [];
  raw.split("\n").forEach((line) => {
    if (!line || line.startsWith(":")) return;    // 空行与注释帧（keepalive）
    if (line.startsWith("event:")) { event = line.slice(6).trim(); return; }
    if (line.startsWith("id:")) { id = line.slice(3).trim(); return; }
    // "retry: N" 与 meta 同帧；浏览器自己读它，前端只需要拿到 id 用于续读。
    if (line.startsWith("data:")) data.push(line.slice(5).trim());
  });
  if (!data.length) return;
  let obj = null;
  try { obj = JSON.parse(data.join("\n")); } catch (e) { obj = { raw: data.join("\n") }; }
  onFrame(event, obj, id);
}

// streamReconnect 在 stream 外面套一层可恢复的订阅。
//
// 后端现在每帧带 id（就是这一行的 offset），流的第一帧还带 retry。断线之后
// 用 Last-Event-ID 把最后收到的位置带回去，服务端从那一行接着发：既不重发整屏、
// 也不把断线期间新打出来的行漏掉。退避从 1 秒到 15 秒，连上过就清零。
// signal 由调用方持有，切页面时一 abort 就彻底停，不会在后台偷偷重连。
export async function streamReconnect(path, handlers, signal) {
  const onFrame = handlers.onFrame || (() => {});
  const onState = handlers.onState || (() => {});
  const getLastId = handlers.getLastId || (() => "");
  const once = !!handlers.once;
  let attempt = 0;
  for (;;) {
    if (signal && signal.aborted) return { ok: true, aborted: true };
    onState("connecting", { attempt });
    const extra = {};
    const id = getLastId();
    if (id !== "" && id !== null && id !== undefined) extra["Last-Event-ID"] = String(id);
    // 收到第一帧才算真的连上。只报 connecting 会让界面一直挂在"连接中"，
    // 而且后端迟迟不写日志时那个转圈也下不来。meta 是首帧，所以不会等太久。
    let connected = false;
    const wrapped = (ev, data, frameId) => {
      if (!connected) {
        connected = true;
        onState("connected", { attempt });
      }
      onFrame(ev, data, frameId);
    };
    const res = await stream(path, wrapped, signal, extra);
    if ((signal && signal.aborted) || res.aborted) return { ok: true, aborted: true };
    if (res.ok) {
      if (once) { onState("closed", {}); return res; }
      // 长期流自己结束就是断线。这里不先报一次 closed：那会让断线牌闪一下再变
      // reconnecting，直接进重连状态更连贯。
      attempt = 0;             // 连上过一次，退避重新从最短开始
    }
    const delay = Math.min(15000, 1000 * Math.pow(2, Math.min(attempt, 4)));
    attempt++;
    onState("reconnecting", { attempt, delay, error: res.error || "日志流断开了，正在重连" });
    await new Promise((resolve) => {
      const t = setTimeout(resolve, delay);
      if (signal) signal.addEventListener("abort", () => { clearTimeout(t); resolve(); }, { once: true });
    });
  }
}

