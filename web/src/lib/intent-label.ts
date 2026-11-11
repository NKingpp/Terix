// intent payload 的展示解析。intent（探索节点）的 payload 是一个 JSON 串，
// 真正的方向描述在 summary 里、执行角色在 role 里；直接渲染原文会看到
// {"summary":"…","asset_ids":[3]} 这种机器串。
//
// 两条规则在这里写一次，会话页签与总览页共用：
//   ① 怎么把 payload 变成可读文字（解析失败回退原文，绝不抛）；
//   ② 哪些 role 不该显示徽标（渗透 intent 的 owner 是 work#N 槽名，不是角色）。

export type IntentLabel = { summary: string; role: string };

// parseIntents 解出摘要与角色。payload 非 JSON / 空串时回退原文，角色为空。
export function parseIntents(payload?: string): IntentLabel {
  const raw = (payload ?? "").trim();
  if (!raw) return { summary: "", role: "" };
  try {
    const p = JSON.parse(raw) as { summary?: unknown; role?: unknown };
    const summary = typeof p?.summary === "string" && p.summary.trim() ? p.summary.trim() : raw;
    const role = typeof p?.role === "string" ? p.role.trim() : "";
    return { summary, role };
  } catch {
    return { summary: raw, role: "" };
  }
}

// isRoleKey 判断这个值是不是真的角色（agent key）。
// 排除空串与 worker 槽名 work#N —— 那是占位，不是角色。
export function isRoleKey(role: string): boolean {
  const v = role.trim();
  return v !== "" && !/^work#\d+$/i.test(v);
}

// roleText 拼展示文案：有中文名则「中文名 key」，否则只有 key。
export function roleText(role: string, names?: Map<string, string>): string {
  const key = role.trim();
  if (!isRoleKey(key)) return "";
  const name = names?.get(key)?.trim();
  return name && name !== key ? `${name} ${key}` : key;
}
