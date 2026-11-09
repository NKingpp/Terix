"use client";

/**
 * UpdateBadge 是顶栏的「有新版本」提示 —— Terix 关闭自更新后它恒不显示。
 *
 * # 为什么保留一个恒返回 null 的组件，而不是删掉调用点
 *
 * 调用点在 `main-content.tsx` 的顶栏布局里。删掉调用点 + 删文件，
 * 会让「顶栏少了东西」这个改动混进布局文件 —— 而布局改动容易误伤。
 *
 * 保留为一个显式的空实现有两个好处：
 *   ① 布局不变，改动面最小
 *   ② 「为什么这里没有更新提示」有地方可查（就是这个注释）
 *
 * # 为什么不保留原来的 checkUpdate 轮询
 *
 * 后端 `/api/update/check` 现在返回 503 + `UPDATE_DISABLED`（见
 * selfupdate.Enabled）。保留轮询的后果是：每次整页加载都发一个必然失败的请求，
 * 然后靠 `.catch(() => {})` 静默吞掉 —— 那是「用错误处理掩盖已删除的功能」。
 *
 * # 何时恢复
 *
 * Terix 建立自己的发布源后，把 `selfupdate.Enabled` 改回 true，
 * 并恢复本组件的 checkUpdate 轮询（可从 git 历史取回）。
 */
export function UpdateBadge() {
  return null;
}
