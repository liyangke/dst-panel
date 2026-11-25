// 写剪贴板。
//
// 面板默认是明文 HTTP 跑在 8942 上（部署文档里 HTTPS 是"建议自己套一层"），
// 而 navigator.clipboard 只在安全上下文里存在：从 http://192.168.1.20:8942 打开时
// 它是 undefined。这时候只能退回 document.execCommand("copy")——那条路已废弃但在
// 这个场景仍然有效，两者都不行时必须把失败说出来，让用户自己选中文本复制。
//
// 返回 true/false 而不是抛错：调用方要据此决定提示写"已复制"还是"请手动复制"，
// 这两种说法在界面上必须不一样，否则用户按了没反应会反复按。
export async function copyText(text) {
  const s = String(text == null ? "" : text);
  if (!s) return false;

  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(s);
      return true;
    } catch (e) {
      // 浏览器拒绝了权限也要试 execCommand：某些浏览器在非用户手势的异步回调里
      // 会 reject clipboard API，而 execCommand 还认这次点击。
    }
  }

  const ta = document.createElement("textarea");
  ta.value = s;
  // readonly 是关键：可编辑的 textarea 在 iOS 上会被聚焦并弹出键盘，
  // 用户看到的"复制成功"其实伴随着输入框被抢走。
  ta.setAttribute("readonly", "");
  ta.style.position = "fixed";
  ta.style.top = "-1000px";
  ta.style.left = "-1000px";
  ta.style.opacity = "0";
  document.body.appendChild(ta);
  ta.select();
  ta.setSelectionRange(0, s.length);
  let ok = false;
  try {
    ok = document.execCommand("copy");
  } catch (e) {
    ok = false;
  }
  document.body.removeChild(ta);
  return ok;
}
