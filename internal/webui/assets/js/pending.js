// 待办条目只有这一份定义：总览「下一步」渲染它，顶栏「待办」入口显示它的条数。
//
// 2026-11-09 第一轮用户："展开后两条提醒的样式不一样……比如待处理提醒合并到下一步这个模块里？"
// 以前这两条各写一段模板：一条带跳转按钮、一条在本页时换成一句提示，
// 还各自带着 hide-sm 和不同长度的话，展开后像两种不同的东西。
// 第二轮用户："总览页面，下一步的卡片已经展示了这些信息，其他页面还需要如图这个吗？看上去很丑"
// ——别页正文顶部那块「摘要行 + 展开清单」也整个下掉，改成顶栏一枚入口。
// 第四轮用户："顶栏的代办……我还是希望能显示数字，他的口径和卡片栏里的待办一致即可。"
// 于是把整份清单（含房间 / 令牌 / 助手 / 备份那几条）抽到 checklistRows：
// 顶栏与总览卡都读同一个函数的返回值，数字不可能再对不上。
//
// 这两个字段都来自 /session（state.me），不额外发请求：
//   - usingDefaultPassword：账号还在用出厂默认密码
//   - engine.present：这台机器上有没有服务端本体
// 房间类那几条读的是 /api/v1/rooms（state.checklist.data），是一次磁盘扫描，
// 由 store.js 每四次轮询核一遍，不在这里发请求。
export function pendingRows(s) {
  const me = (s && s.me) || {};
  const out = [];

  if (me.usingDefaultPassword) {
    out.push({
      key: "password",
      icon: "shield",
      tint: "tint-coral",
      tag: "安全",
      short: "还在用出厂默认密码",
      text: "这个账号还在用出厂默认密码，面板又直接开在公网端口上；先去账号管理改掉它。",
      btn: "去改密码",
      href: "#/accounts",
      tail: "就在账号管理页最下面「修改我的密码」那一栏。",
    });
  }

  const e = me.engine;
  if (e && !e.present) {
    const downloadable = !!e.steamCmdReady;
    out.push({
      key: "engine",
      icon: "download",
      tint: "tint-coral",
      tag: "环境",
      short: "没有服务端本体",
      // 缺本体挡的是「启动」，不是「建房间」：建房间只写 kleiRoot 下的文件，
      // 所以这句话不能说成"房间建不了"，否则新手会站在提醒前不敢往下走。
      text: downloadable
        ? "服务端本体还没装，房间建好也起不了服。下载要占十几分钟，可以先让它跑着，这期间去建房间。"
        : "服务端本体还没装，下载工具也没就位（" + (e.steamCmdNote || "没配 steamCmd 路径") +
          "），只能自己把本体装到 " + (e.installDir || "配置里的 dstInstall") + "。",
      btn: downloadable ? "去任务中心下载" : "看面板设置",
      href: downloadable ? "#/tasks" : "#/settings",
      tail: "在任务中心那张提交卡上直接提交下载。",
    });
  }
  return out;
}

// 完整清单 = 上面两条全局提醒 + 管理员才有的机器现状核对。
// s 是 store 的 state；rooms 是 /api/v1/rooms 的返回（null 表示还没核过，或只读账号不扫盘）。
// 只读账号也能看见安全 / 环境两条（安全提醒不挑角色），只是总览把动作换成一句说明。
export function checklistRows(s, rooms) {
  const st = s || {};
  const out = pendingRows(st);
  if (!(st.me && st.me.isAdmin)) return out;
  if (!rooms) return out;
  const clusters = st.clusters || [];
  const rows = rooms.items || [];

  // 一条都不缺时返回空数组：总览卡据此收成一行「都齐了」，顶栏据此不弹入口。
  // 顺序就是挡路的程度：没房间时后面几条全是噪音，所以直接返回。
  if (!clusters.length) {
    out.push({ key: "room", icon: "plus", tint: "tint-azure",
      short: "这台机器上还没有房间",
      text: "这台机器上还没有房间。开服向导一次配齐目录、端口、世界规则和助手 MOD。",
      btn: "去开服向导", href: "#/room" });
    return out;
  }
  const idle = clusters.filter((c) => !(c.shards || []).some((x) => x.running));
  if (idle.length === clusters.length) {
    out.push({ key: "start", icon: "play", tint: "tint-green",
      short: clusters.length + " 个房间都没在跑",
      text: clusters.length + " 个房间的世界现在都没在跑，玩家进不来。",
      btn: "去启动", href: "#/room" });
  }
  const noToken = rows.filter((x) => !x.hasToken);
  if (noToken.length) {
    out.push({ key: "token", icon: "shield", tint: "tint-gold",
      short: noToken.length + " 个世界没填令牌",
      text: noToken.length + " 个世界没填 Klei 令牌：玩家在饥荒的服务器列表里搜不到它" +
        "（把 IP 和端口发给朋友照样能连）。",
      btn: "去填令牌", href: "#/room?tab=token" });
  }
  // 只数真故障：没人时引擎自己把世界冻住，助手随之停摆，那是设计如此。
  // 把暂停也算进来，这条提醒就会每天挂在首页上，用户两天之后不再看它。
  const quiet = rows.filter((x) => x.running && !x.helperOnline && x.helperNoteUrgent);
  if (quiet.length) {
    out.push({ key: "helper", icon: "cube", tint: "tint-violet",
      short: quiet.length + " 个世界助手没回话",
      text: quiet.length + " 个世界在跑但助手没回话：面板看不到在线人数、天数和地图。",
      btn: "看模组与助手", href: "#/mods" });
  }
  const allRooms = new Set(rows.map((x) => x.cluster));
  const backed = new Set(rows.filter((x) => x.lastBackupAt).map((x) => x.cluster));
  const noBackup = [...allRooms].filter((c) => !backed.has(c));
  if (noBackup.length) {
    out.push({ key: "backup", icon: "archive", tint: "tint-cyan",
      short: noBackup.length + " 个房间没备份过",
      text: noBackup.length + " 个房间从来没有备份过。存档坏了只能重开，先备份一次再说。",
      btn: "去备份", href: "#/snaps" });
  }
  return out;
}

// 顶栏那枚入口的悬停摘要：每行都带 short，拼出来就是"有几条、都是什么"。
export function pendingSummary(rows) {
  return (rows || []).map((r) => r.short || r.tag || "").filter(Boolean).join("、");
}
