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

import { useExecMode } from "./_components/exec-mode-provider";

const MODE_OPTIONS = [
  { value: "paper", label: "模拟盘（paper）", hint: "自动成交，仅记纸面账本" },
  { value: "live_whitelist", label: "实盘·白名单", hint: "只允许白名单标的" },
  { value: "live_manual", label: "实盘·人工确认", hint: "每笔都要人工确认" },
  // 2026-11-05：实盘自动。提示文案必须说清「不询问」——
  // 这是用户最容易误判的一点（以为还会弹确认框）。
  { value: "live_auto", label: "实盘·自动", hint: "系统自行下单，不再逐笔询问" },
  { value: "frozen", label: "冻结", hint: "停止一切自动执行" },
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
          {frozen ? (
            <Alert variant="destructive">
              <LockIcon className="size-4" />
              <AlertTitle>系统已冻结</AlertTitle>
              <AlertDescription>
                冻结状态下不会执行任何自动交易（含 paper）。切换模式已禁用，需先解冻（要求先回 paper）。
              </AlertDescription>
            </Alert>
          ) : null}

          {/* 切模式：frozen 时全部禁用 —— 不只是显示横幅，按钮也要禁掉。
              否则用户点了才发现失败，还得读一遍 409 的原因。 */}
          <div className="flex flex-wrap items-end gap-3">
            <div className="w-56">
              <Label>切换模式</Label>
              <Select value={targetMode} onValueChange={setTargetMode} disabled={frozen || busy}>
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
            <Button disabled={!targetMode || frozen || busy} onClick={() => void doSetMode(targetMode)}>
              应用
            </Button>
          </div>

          <p className="text-xs text-muted-foreground">
            选择目标模式后点「应用」即直接切换，无需授权实盘或限额确认。
            模拟盘（paper）会把已过闸的草案自动成交为纸面订单；实盘·自动会下发真实通道。
          </p>
        </CardContent>
      </Card>

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
