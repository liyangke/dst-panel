// 数字、时间、状态的"说法"集中在这里。
//
// 为什么不让每个页面自己拼：接口给的字节数是整数、时间是 RFC3339 的 UTC，
// 页面各写一套的话，"140MB"、"140.0 MB"、"1.4 亿字节"会在同一屏里同时出现，
// 而 UTC 时间如果被按字符串截断显示，用户看到的是比本机早八点的钟点。
const UNITS = ["B", "KB", "MB", "GB", "TB"];

// bytes 把字节数说成能读的宽度。1023 B 保持 B，因为"0.99 KB"没人念。
export function bytes(n) {
  if (n === null || n === undefined || Number.isNaN(Number(n))) return "—";
  let v = Number(n);
  if (v < 1024) return v + " B";
  let i = 0;
  while (v >= 1024 && i < UNITS.length - 1) { v /= 1024; i++; }
  // 保留一位小数只在真需要的时候：128 MB 写成 128.0 MB 是噪音。
  const s = v >= 100 || Number.isInteger(v) ? String(Math.round(v)) : v.toFixed(1);
  return s + " " + UNITS[i];
}

// bytesPair 给"已下 / 整包"这一对并排的数字：两格必须同一档单位。
// 各按各的量取单位会排成"1022 MB / 4.3 GB"，读者得先心算一次才知道谁多。
// 单位按大的那个取，小的那位因此多留一位（1.02 GB）。
export function bytesPair(a, b) {
  const ref = Math.max(Number(a) || 0, Number(b) || 0);
  let s = ref, i = 0;
  while (s >= 1024 && i < UNITS.length - 1) { s /= 1024; i++; }
  const say = (n) => {
    const v = Number(n);
    if (!Number.isFinite(v)) return "—";
    const q = v / Math.pow(1024, i);
    return (q >= 100 ? Math.round(q) : Number.isInteger(q) ? q : q.toFixed(q >= 10 ? 1 : 2)) + " " + UNITS[i];
  };
  return [say(a), say(b)];
}

// num 给整数加千分位。进度、文件数、磁盘余量都走它。
export function num(n) {
  if (n === null || n === undefined || n === "") return "—";
  const v = Number(n);
  return Number.isNaN(v) ? String(n) : v.toLocaleString("zh-CN");
}

// count 给"一个人数字段要排在很窄的地方"的场合（卡片上的热度条旁边那一格）：
// 八位数的千分位写法会把条挤没，而中文读者本来就读"9000 万"。
// 这里是有损的，所以精确值必须另有一处能读到（详情层、悬停标题）。
//
// 最多留三个有效数字，而且一定带单位：不封顶的话 1044.5 万 和 850 万 会一个七字一个四字，
// 挨着它们的条就被挤成长短不一——同一页里排着的条必须能横向比长短，这是它唯一的作用。
export function count(n) {
  const v = Number(n);
  if (!Number.isFinite(v)) return "—";
  const a = Math.abs(v);
  if (a < 1e4) return num(v);
  if (a < 1e8) return sig(v / 1e4) + " 万";
  return sig(v / 1e8) + " 亿";
}

// sig 把"除以万/亿之后"的商折成最多三位有效数字，并且不留 ".0" 这种尾巴。
function sig(q) {
  const d = Math.abs(q) < 10 ? 2 : Math.abs(q) < 100 ? 1 : 0;
  const f = Math.pow(10, d);
  return String(Math.round(q * f) / f);
}

export function pct(a, b) {
  if (!b) return 0;
  const p = (Number(a) / Number(b)) * 100;
  if (Number.isNaN(p)) return 0;
  return Math.max(0, Math.min(100, p));
}

function asDate(v) {
  if (v instanceof Date) return v;
  if (typeof v === "number") return new Date(v);
  if (!v) return null;
  const d = new Date(v);
  return Number.isNaN(d.getTime()) ? null : d;
}

const pad = (n) => String(n).padStart(2, "0");

// clock 给"几点几分"，用浏览器所在时区：面板是给运维看的，他要知道的是自己表上的时间。
export function clock(v, withSeconds) {
  const d = asDate(v);
  if (!d) return "—";
  const t = pad(d.getHours()) + ":" + pad(d.getMinutes());
  return withSeconds ? t + ":" + pad(d.getSeconds()) : t;
}

export function dateText(v) {
  const d = asDate(v);
  if (!d) return "—";
  return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate());
}

export function dateTime(v) {
  const d = asDate(v);
  if (!d) return "—";
  return dateText(d) + " " + clock(d, true);
}

// ago 给"多久以前"。秒级精度只在半分钟内有用，再细就变成每帧换行的闪烁。
export function ago(v) {
  const d = asDate(v);
  if (!d) return "—";
  const s = Math.round((Date.now() - d.getTime()) / 1000);
  if (s < 0) return "刚刚";
  if (s < 60) return s + " 秒前";
  if (s < 3600) return Math.floor(s / 60) + " 分钟前";
  if (s < 86400) return Math.floor(s / 3600) + " 小时前";
  if (s < 86400 * 30) return Math.floor(s / 86400) + " 天前";
  return dateText(d);
}

// seconds 给一段时长（任务的耗时、备份间隔）。传接口的秒数。
export function seconds(s) {
  const n = Number(s);
  if (!Number.isFinite(n) || n < 0) return "—";
  // 一秒以内的任务不少（恢复一份小快照、取消一个空队列），
  // "0.0 秒"看着像没跑；说"不到 1 秒"才是实话。
  if (n < 1) return "不到 1 秒";
  if (n < 60) return (n < 10 ? n.toFixed(1) : Math.round(n)) + " 秒";
  if (n < 3600) return Math.floor(n / 60) + " 分 " + pad(Math.round(n % 60)) + " 秒";
  return Math.floor(n / 3600) + " 小时 " + pad(Math.floor((n % 3600) / 60)) + " 分";
}

// key 是分片的主键："Cluster_1/Master"。状态缓存与选择都按它索引。
export function keyOf(cluster, shard) { return cluster + "/" + shard; }

export function parseKey(k) {
  const i = String(k).indexOf("/");
  return i < 0 ? { cluster: k, shard: "" } : { cluster: k.slice(0, i), shard: k.slice(i + 1) };
}

// health 这类三元组：接口用"字段缺没缺"表达"MOD 没采到这个组件"，
// 所以这里必须区分 null 和 0——0 会被读成"快饿死了"。
export function statText(v) {
  if (v === null || v === undefined) return "未知";
  return String(Math.round(Number(v)));
}

export function coord(x, z) {
  if (x === null || x === undefined || z === null || z === undefined) return "未知";
  return Math.round(Number(x)) + ", " + Math.round(Number(z));
}
