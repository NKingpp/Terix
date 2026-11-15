"use client";

// 执行模式页（WE-13）。
//
// # 这个页面要回答的三个问题
//
//   1. 现在是什么模式？—— 徽章 + 指纹 + 限额版本
//   2. 能不能改？—— 切模式按钮（frozen 时禁用）
//   3. 改限额要确认什么？—— 当前值 → 新值 + 确认短语
//
// # 为什么「当前值」必须显式展示
//
// 限额是「能亏多少钱」。用户在改之前必须看到「现在是 1 万」，
// 否则他不知道自己在改什么。
//
// 而 audit 的 detail 里也有 before/after，两者是同一件事的两种呈现：
//   前端：让用户在改之前看到
//   审计：让用户在事后核查

import * as React from "react";

import { LockIcon, ShieldOffIcon } from "lucide-react";
import { toast } from "sonner";

import { StatusBadge } from "@/components/status-badge";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { api, type ExecLimits, type ExecOrder, errCode } from "@/lib/api";
import type { ExecAccount, TradeLoopStatus } from "@/lib/types";

import { useExecMode } from "./_components/exec-mode-provider";

// 只有实盘三档。纸面模式与冻结状态已删除 —— 这是无人值守系统，
// 「停下来」靠关任务或拉黑账户，不靠一个模式值。
const MODE_OPTIONS = [
  { value: "live_whitelist", label: "实盘·白名单", hint: "只允许白名单标的" },
  { value: "live_manual", label: "实盘·人工确认", hint: "每笔都要人工确认" },
  { value: "live_auto", label: "实盘·自动", hint: "系统自行下单，不再逐笔询问" },
];

export default function ExecPage() {
  const { mode, frozen, fingerprint, limitsRevision, loaded, reload } = useExecMode();

  const [limits, setLimits] = React.useState<ExecLimits | null>(null);
  const [orders, setOrders] = React.useState<ExecOrder[]>([]);
  const [busy, setBusy] = React.useState(false);

  // 待切换的目标模式（用户选了但还没确认）
  const [targetMode, setTargetMode] = React.useState("");

  const loadLimits = React.useCallback(async () => {
    try {
      setLimits(await api.execLimits());
    } catch (e) {
      toast.error(`读限额失败：${(e as Error).message}`);
    }
  }, []);

  const loadOrders = React.useCallback(async () => {
    try {
      const r = await api.execOrders({ limit: 20 });
      setOrders(r.orders);
    } catch (e) {
      toast.error(`读执行单失败：${(e as Error).message}`);
    }
  }, []);

  React.useEffect(() => {
    void loadLimits();
    void loadOrders();
  }, [loadLimits, loadOrders]);

  async function doSetMode(to: string) {
    setBusy(true);
    try {
      await api.setExecMode(to);
      toast.success(`已切换到 ${to}`);
      setTargetMode("");
      reload();
    } catch (e) {
      // 409 = 状态冲突（如 frozen→live），错误文案已由后端给出
      const code = errCode(e);
      toast.error(code ? `${toastText(code)}：${(e as Error).message}` : (e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  if (!loaded) {
    return (
      <div className="space-y-4 p-6">
        <Skeleton className="h-24 w-full" />
        <Skeleton className="h-48 w-full" />
      </div>
    );
  }

  return (
    <div className="space-y-6 p-6">
      {/* ---- 执行态总览 ---- */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            执行态
            <StatusBadge domain="execMode" value={mode ?? ""} dot />
          </CardTitle>
          <CardDescription>
            规则指纹 <code className="text-xs">{fingerprint || "—"}</code>
            {" · "}限额版本 <code className="text-xs">{limitsRevision ?? 0}</code>
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex flex-wrap items-end gap-3">
            <div className="w-56">
              <Label>切换模式</Label>
              <Select value={targetMode} onValueChange={setTargetMode} disabled={busy}>
                <SelectTrigger>
                  <SelectValue placeholder="选择目标模式" />
                </SelectTrigger>
                <SelectContent>
                  {MODE_OPTIONS.map((o) => (
                    <SelectItem key={o.value} value={o.value} disabled={o.value === mode}>
                      {o.label} — {o.hint}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <Button disabled={!targetMode || busy} onClick={() => void doSetMode(targetMode)}>
              应用
            </Button>
          </div>

          <p className="text-xs text-muted-foreground">
            选择目标模式后点「应用」即直接切换。实盘·自动会下发真实通道。
          </p>
        </CardContent>
      </Card>

      {/* ---- 执行账户 ---- */}
      <AccountsCard />

      {/* ---- 交易循环状态 ---- */}
      <LoopStatusCard />

      {/* ---- 限额 ---- */}
      <LimitsCard
        limits={limits}
        frozen={frozen}
        onSaved={() => {
          void loadLimits();
          reload();
        }}
      />

      {/* ---- 执行单列表 ---- */}
      <Card>
        <CardHeader>
          <CardTitle>最近执行单</CardTitle>
          <CardDescription>最多 20 条。状态与风险判定的关联见 P1 的详情页。</CardDescription>
        </CardHeader>
        <CardContent>
          {orders.length === 0 ? (
            <p className="text-sm text-muted-foreground">暂无执行记录。</p>
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b text-left text-muted-foreground">
                    <th className="py-2 pr-3 font-medium">#</th>
                    <th className="py-2 pr-3 font-medium">状态</th>
                    <th className="py-2 pr-3 font-medium">模式</th>
                    <th className="py-2 pr-3 font-medium">数量</th>
                    <th className="py-2 pr-3 font-medium">已成交</th>
                    <th className="py-2 pr-3 font-medium">均价</th>
                    <th className="py-2 font-medium">时间</th>
                  </tr>
                </thead>
                <tbody>
                  {orders.map((o) => (
                    <tr key={o.id} className="border-b last:border-0">
                      <td className="py-2 pr-3 font-mono text-xs">{o.id}</td>
                      <td className="py-2 pr-3">
                        <Badge variant="outline">{o.state}</Badge>
                      </td>
                      <td className="py-2 pr-3">
                        <StatusBadge domain="execMode" value={o.mode} />
                      </td>
                      <td className="py-2 pr-3 font-mono text-xs">{o.qty}</td>
                      <td className="py-2 pr-3 font-mono text-xs">{o.filled_qty}</td>
                      <td className="py-2 pr-3 font-mono text-xs">{o.avg_price ?? "—"}</td>
                      <td className="py-2 text-xs text-muted-foreground">{o.state_at}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </CardContent>
      </Card>

    </div>
  );
}

// ---- 限额卡片 ----

// 确认短语必须与后端常量**逐字相同**（后端是精确匹配，不 trim）。
const LIMITS_PHRASE = "我确认修改交易限额";

function LimitsCard({ limits, frozen, onSaved }: { limits: ExecLimits | null; frozen: boolean; onSaved: () => void }) {
  const [single, setSingle] = React.useState("");
  const [daily, setDaily] = React.useState("");
  const [orders, setOrders] = React.useState("");
  const [phrase, setPhrase] = React.useState("");
  const [busy, setBusy] = React.useState(false);

  // 限额拉到后同步到表单（作为编辑起点）。
  React.useEffect(() => {
    if (!limits) return;
    setSingle(limits.max_single_amount ?? "");
    setDaily(limits.max_daily_amount ?? "");
    setOrders(limits.max_daily_orders == null ? "" : String(limits.max_daily_orders));
  }, [limits]);

  const exactMatch = phrase === LIMITS_PHRASE;

  async function save() {
    setBusy(true);
    try {
      await api.setExecLimits({
        maxSingleAmount: single.trim(),
        maxDailyAmount: daily.trim(),
        maxDailyOrders: Number(orders),
        confirmPhrase: phrase,
      });
      toast.success("限额已更新");
      setPhrase("");
      onSaved();
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>交易限额</CardTitle>
        <CardDescription>当前值 → 新值会同时写入审计表（含 before/after），事后可查。</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {/* 当前值 vs 新值：并排显示，让用户看到「在改什么」 */}
        <div className="grid gap-4 sm:grid-cols-3">
          <LimitField
            label="单笔上限"
            current={limits?.max_single_amount}
            value={single}
            onChange={setSingle}
            disabled={frozen || busy}
          />
          <LimitField
            label="日累计上限"
            current={limits?.max_daily_amount}
            value={daily}
            onChange={setDaily}
            disabled={frozen || busy}
          />
          <LimitField
            label="日单数上限"
            current={limits?.max_daily_orders == null ? null : String(limits.max_daily_orders)}
            value={orders}
            onChange={setOrders}
            disabled={frozen || busy}
          />
        </div>

        <div className="space-y-2">
          <Label htmlFor="limits-phrase">确认短语（逐字输入「{LIMITS_PHRASE}」）</Label>
          <Input
            id="limits-phrase"
            value={phrase}
            onChange={(e) => setPhrase(e.target.value)}
            placeholder={LIMITS_PHRASE}
            disabled={frozen || busy}
          />
          <p className="text-xs text-muted-foreground">
            需逐字匹配（区分空格）。这是为了让「改限额」成为一次有意识的动作， 而不是一次误点。
          </p>
        </div>

        <Button disabled={!exactMatch || frozen || busy} onClick={() => void save()}>
          <ShieldOffIcon className="size-4" />
          保存限额
        </Button>
      </CardContent>
    </Card>
  );
}

function LimitField({
  label,
  current,
  value,
  onChange,
  disabled,
}: {
  label: string;
  current?: string | null;
  value: string;
  onChange: (v: string) => void;
  disabled?: boolean;
}) {
  const changed = (current ?? "") !== value;
  return (
    <div className="space-y-1">
      <Label>{label}</Label>
      <Input value={value} onChange={(e) => onChange(e.target.value)} disabled={disabled} inputMode="decimal" />
      <p className="text-xs text-muted-foreground">
        当前：<span className={changed ? "font-medium text-amber-600" : ""}>{current ?? "未配置"}</span>
        {changed ? " → 待保存" : null}
      </p>
    </div>
  );
}

// toastText 把后端错误码翻成一句人话前缀。
//
// 不覆盖所有码 —— 覆盖那些「用户需要知道该做什么」的。
function toastText(code: string): string {
  switch (code) {
    case "EXEC_MODE_FROZEN":
      return "系统已冻结，需先解冻";
    case "EXEC_LIMITS_INCOMPLETE":
      return "限额未配置完整";
    case "EXEC_CONFIRM_MISMATCH":
      return "确认短语不匹配";
    case "EXEC_LIMITS_CONTRADICTORY":
      return "限额自相矛盾";
    default:
      return code;
  }
}

// ---- 执行账户卡片 ----
//
// 执行账户是「服务可以对哪个真实账户下单」的唯一声明，必须人工创建：
// 不给 uid 时后端会用当前 OKX 凭证探测一次（拿不到 uid 说明凭证有问题，
// 此时建档只会造出一个「看起来能用但下不了单」的账户）。
function AccountsCard() {
  const [accounts, setAccounts] = React.useState<ExecAccount[]>([]);
  const [label, setLabel] = React.useState("");
  const [uid, setUid] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [err, setErr] = React.useState("");

  const load = React.useCallback(async () => {
    try {
      const resp = await api.execAccounts();
      setAccounts(resp.accounts);
    } catch {
      // 忽略：瞬时错误，下次轮询重试
    }
  }, []);

  React.useEffect(() => {
    void load();
    const t = setInterval(() => void load(), 10000);
    return () => clearInterval(t);
  }, [load]);

  async function create() {
    if (!label.trim()) {
      setErr("请填写账户标签");
      return;
    }
    setBusy(true);
    setErr("");
    try {
      await api.createExecAccount({ label: label.trim(), channelAccountId: uid.trim() || undefined });
      setLabel("");
      setUid("");
      await load();
      toast.success("执行账户已创建");
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function toggle(a: ExecAccount) {
    try {
      await api.patchExecAccount(a.id, { active: !a.active });
      await load();
    } catch (e) {
      toast.error((e as Error).message);
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>执行账户</CardTitle>
        <CardDescription>
          交易循环只对这里启用的 OKX 账户下单。paper-auto 账户是纸面占位，不会被使用。
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex flex-wrap items-end gap-2">
          <div className="flex flex-col gap-1">
            <Label className="text-xs">账户标签</Label>
            <Input className="h-8 w-40" value={label} onChange={(e) => setLabel(e.target.value)} placeholder="如 okx-main" />
          </div>
          <div className="flex flex-col gap-1">
            <Label className="text-xs">OKX uid（留空自动探测）</Label>
            <Input className="h-8 w-48" value={uid} onChange={(e) => setUid(e.target.value)} placeholder="留空则用凭证探测" />
          </div>
          <Button size="sm" disabled={busy} onClick={() => void create()}>
            创建
          </Button>
          {err && <span className="text-xs text-red-500">{err}</span>}
        </div>
        {accounts.length === 0 ? (
          <p className="text-sm text-muted-foreground">还没有执行账户。实盘下单前必须先创建一个。</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b text-left text-muted-foreground">
                  <th className="py-2 pr-3 font-medium">#</th>
                  <th className="py-2 pr-3 font-medium">标签</th>
                  <th className="py-2 pr-3 font-medium">uid</th>
                  <th className="py-2 pr-3 font-medium">市场</th>
                  <th className="py-2 pr-3 font-medium">仓位模式</th>
                  <th className="py-2 pr-3 font-medium">杠杆</th>
                  <th className="py-2 pr-3 font-medium">跟踪持仓</th>
                  <th className="py-2 font-medium">状态</th>
                </tr>
              </thead>
              <tbody>
                {accounts.map((a) => (
                  <tr key={a.id} className="border-b last:border-0">
                    <td className="py-2 pr-3 font-mono text-xs">{a.id}</td>
                    <td className="py-2 pr-3">{a.label || "—"}</td>
                    <td className="py-2 pr-3 font-mono text-xs">{a.channel_account_id}</td>
                    <td className="py-2 pr-3 text-xs">{(a.market_support ?? []).join("/") || a.market}</td>
                    <td className="py-2 pr-3 text-xs">{a.pos_mode} · {a.mgn_mode}</td>
                    <td className="py-2 pr-3 font-mono text-xs">{a.lever}x</td>
                    <td className="py-2 pr-3 font-mono text-xs">{a.open_tracked}</td>
                    <td className="py-2">
                      <Button size="sm" variant="outline" className="h-6 px-2 text-xs" onClick={() => void toggle(a)}>
                        {a.blocked_at ? "已冻结" : a.active ? "启用中" : "已停用"}
                      </Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

// ---- 交易循环状态卡片 ----
//
// 这个卡片要回答的是「无人值守到底有没有在跑」：模式、持仓数、
// 最近一次 tick、最后一个错误。没有它，出问题只能翻容器日志。
function LoopStatusCard() {
  const [st, setSt] = React.useState<TradeLoopStatus | null>(null);

  React.useEffect(() => {
    let cancelled = false;
    const load = async () => {
      try {
        const resp = await api.execLoopStatus();
        if (!cancelled) setSt(resp);
      } catch {
        // 忽略
      }
    };
    void load();
    const t = setInterval(() => void load(), 10000);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, []);

  const running = st?.running ?? false;
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          交易循环
          <Badge variant={running ? "default" : "outline"}>{running ? "运行中" : "未启用"}</Badge>
        </CardTitle>
        <CardDescription>
          live_auto 模式下每 {st?.interval_sec ?? 15} 秒读持仓、补挂 TP/SL、消费已过闸草案开仓。
        </CardDescription>
      </CardHeader>
      <CardContent className="grid grid-cols-2 gap-3 text-sm md:grid-cols-4">
        <div>
          <div className="text-xs text-muted-foreground">当前模式</div>
          <div className="font-mono text-xs">{st?.mode || "—"}</div>
        </div>
        <div>
          <div className="text-xs text-muted-foreground">交易所持仓</div>
          <div className="font-mono text-xs">{st?.positions ?? 0}</div>
        </div>
        <div>
          <div className="text-xs text-muted-foreground">跟踪中</div>
          <div className="font-mono text-xs">{st?.open_tracked ?? 0}</div>
        </div>
        <div>
          <div className="text-xs text-muted-foreground">最近 tick</div>
          <div className="font-mono text-xs">{st?.last_tick ? new Date(st.last_tick).toLocaleTimeString("zh-CN") : "—"}</div>
        </div>
        {st?.last_error && (
          <div className="col-span-2 md:col-span-4">
            <div className="text-xs text-muted-foreground">最后错误</div>
            <div className="text-xs text-red-500 break-words">{st.last_error}</div>
          </div>
        )}
      </CardContent>
    </Card>
  );
}
