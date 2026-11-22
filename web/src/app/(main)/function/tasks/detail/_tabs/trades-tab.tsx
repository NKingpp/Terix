"use client";

// 交易记录 Tab：本任务的成交 + 按标的汇总的已实现盈亏。
//
// # 为什么实盘与模拟盘分两块
//
// 两边的钱不是一回事。混在一起算总额会得出「模拟盘赚 50 + 实盘亏 30 = 赚 20」
// 这种没意义的数字。所以两块各算各的，绝不合并。
//
// # 为什么盈亏由后端算
//
// 均价 × 总量算不出手续费，也算不出分批平仓的实际成本。后端用
// 加权平均成本法逐笔配对（exec/pnl.go），实盘与模拟盘共用同一套算法 ——
// 这才是「模拟盘能验证策略」的前提。

import * as React from "react";

import { RefreshCwIcon, TrendingDownIcon, TrendingUpIcon, WalletIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { api } from "@/lib/api";
import type { TaskTrades, TradeAction, TradePnL, TradeTotals } from "@/lib/types";

function fmtSigned(v: string): string {
  const n = Number(v);
  if (!Number.isFinite(n)) return v;
  if (n === 0) return "0.00";
  const s = n.toFixed(2);
  return n > 0 ? `+${s}` : s;
}

function fmtPlain(v: string, digits = 4): string {
  const n = Number(v);
  if (!Number.isFinite(n)) return v;
  return n.toFixed(digits);
}

// 涨红跌绿（中国习惯），不要改成西方配色。
function toneClass(v: string): string {
  const n = Number(v);
  if (!Number.isFinite(n) || n === 0) return "text-muted-foreground";
  return n > 0 ? "text-red-500" : "text-green-600";
}

function TotalsRow({ totals, feeNote }: { totals: TradeTotals; feeNote: string }) {
  return (
    <div className="grid grid-cols-2 gap-4 md:grid-cols-4">
      <div>
        <div className="text-muted-foreground text-xs">已实现盈亏</div>
        <div className={`font-mono text-lg ${toneClass(totals.realized)}`}>{fmtSigned(totals.realized)}</div>
      </div>
      <div>
        <div className="text-muted-foreground text-xs">手续费</div>
        <div className="font-mono text-lg text-muted-foreground">{fmtPlain(totals.fee_total, 4)}</div>
      </div>
      <div>
        <div className="text-muted-foreground text-xs">净盈亏</div>
        <div className={`font-mono text-lg font-medium ${toneClass(totals.net_realized)}`}>
          {fmtSigned(totals.net_realized)}
        </div>
      </div>
      <div>
        <div className="text-muted-foreground text-xs">口径</div>
        <div className="text-xs text-muted-foreground">{feeNote}</div>
      </div>
    </div>
  );
}

function PnLTable({ pnl }: { pnl: TradePnL[] }) {
  if (pnl.length === 0) return <p className="text-muted-foreground text-sm">暂无标的盈亏数据。</p>;
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b text-left text-muted-foreground">
            <th className="py-2 pr-3 font-medium">标的</th>
            <th className="py-2 pr-3 font-medium">笔数</th>
            <th className="py-2 pr-3 font-medium">买入额</th>
            <th className="py-2 pr-3 font-medium">卖出额</th>
            <th className="py-2 pr-3 font-medium">已实现</th>
            <th className="py-2 pr-3 font-medium">手续费</th>
            <th className="py-2 pr-3 font-medium">净盈亏</th>
            <th className="py-2 pr-3 font-medium">持仓</th>
            <th className="py-2 font-medium">持仓均价</th>
          </tr>
        </thead>
        <tbody>
          {pnl.map((p) => {
            const open = Number(p.open_qty);
            return (
              <tr key={`${p.inst_id}:${p.pos_side || ""}`} className="border-b last:border-0">
                <td className="py-2 pr-3 font-mono text-xs">{p.inst_id}{p.pos_side ? ` · ${p.pos_side === "short" ? "空" : "多"}` : ""}</td>
                <td className="py-2 pr-3 font-mono text-xs">{p.fill_count}</td>
                <td className="py-2 pr-3 font-mono text-xs">{fmtPlain(p.buy_amount, 2)}</td>
                <td className="py-2 pr-3 font-mono text-xs">{fmtPlain(p.sell_amount, 2)}</td>
                <td className={`py-2 pr-3 font-mono text-xs ${toneClass(p.realized)}`}>{fmtSigned(p.realized)}</td>
                <td className="py-2 pr-3 font-mono text-xs text-muted-foreground">{fmtPlain(p.fee_total, 4)}</td>
                <td className={`py-2 pr-3 font-mono text-xs font-medium ${toneClass(p.net_realized)}`}>
                  {fmtSigned(p.net_realized)}
                </td>
                <td className="py-2 pr-3 font-mono text-xs">
                  {open === 0 ? (
                    <span className="text-muted-foreground">已平</span>
                  ) : (
                    <span className={open > 0 ? "text-red-500" : "text-green-600"}>
                      {open > 0 ? (
                        <TrendingUpIcon className="mr-1 inline size-3" />
                      ) : (
                        <TrendingDownIcon className="mr-1 inline size-3" />
                      )}
                      {p.open_qty}
                    </span>
                  )}
                </td>
                <td className="py-2 font-mono text-xs text-muted-foreground">
                  {p.open_avg_cost ? fmtPlain(p.open_avg_cost, 4) : "—"}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

export function TradesTab({ taskId }: { taskId: string }) {
  const [data, setData] = React.useState<TaskTrades | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [syncing, setSyncing] = React.useState(false);

  const load = React.useCallback(async () => {
    try {
      setData(await api.taskTrades(taskId));
    } catch {
      setData(null);
    } finally {
      setLoading(false);
    }
  }, [taskId]);

  React.useEffect(() => {
    void load();
    const t = setInterval(() => void load(), 20000);
    return () => clearInterval(t);
  }, [load]);

  async function sync() {
    setSyncing(true);
    try {
      const r = await api.taskTradesSync(taskId);
      if (r.warning) toast.warning(`已同步 ${r.inserted} 笔，但有告警：${r.warning}`);
      else if (r.inserted > 0) toast.success(`已同步 ${r.inserted} 笔新成交`);
      else toast.info("没有新成交");
      await load();
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setSyncing(false);
    }
  }

  if (loading) {
    return <p className="text-muted-foreground text-sm">加载中…</p>;
  }

  const live = data?.live;
  // 后端本该发 [] 而非 null（server/trades_api.go 已修），但这里仍要兜底：
  // `live?.fills.length` 只护了 live，fills 为 null 时属性访问直接抛，
  // 整页变成 "This page couldn't load"。可选链要护到最内层。
  const liveFills = live?.fills ?? [];
  const liveCount = liveFills.length;

  return (
    <div className="flex flex-col gap-4">
      {data?.note && (
        <p className="rounded-md border border-dashed px-3 py-2 text-muted-foreground text-xs">{data.note}</p>
      )}

      {/* ---- 实盘 ---- */}
      <Card>
        <CardHeader className="flex-row items-start justify-between gap-4">
          <div>
            <CardTitle className="flex items-center gap-2 text-sm">
              <WalletIcon className="size-4" /> 实盘
              <Badge variant={liveCount > 0 ? "default" : "outline"}>
                {liveCount > 0 ? `${liveCount} 笔成交` : "无成交"}
              </Badge>
            </CardTitle>
            <CardDescription>
              交易所逐笔成交（含手续费）。每 60 秒自动同步；持仓中的浮盈不计入已实现盈亏。
            </CardDescription>
          </div>
          <Button size="sm" variant="outline" disabled={syncing} onClick={() => void sync()}>
            <RefreshCwIcon className={syncing ? "animate-spin" : ""} /> 立即同步
          </Button>
        </CardHeader>
        <CardContent className="space-y-4">
          <TotalsRow totals={live?.totals ?? { realized: "0", fee_total: "0", net_realized: "0" }} feeNote="含手续费" />
          <PnLTable pnl={live?.pnl ?? []} />
        </CardContent>
      </Card>

      {/* ---- 交易台账（工具动作） ---- */}
      <Card>
        <CardHeader>
          <CardTitle className="text-sm">交易台账</CardTitle>
          <CardDescription>
            每一次开仓 / 平仓 / 调杠杆的原始动作，含**被拒的**。与上方成交明细的差别：
            一笔市价单可能拆成多笔成交，也可能失败而没有任何成交。
          </CardDescription>
        </CardHeader>
        <CardContent>
          {(data?.actions?.length ?? 0) === 0 ? (
            <p className="text-muted-foreground text-sm">本任务还没有工具下单记录。</p>
          ) : (
            <div className="max-h-[45vh] overflow-auto">
              <table className="w-full text-sm">
                <thead className="sticky top-0 bg-background">
                  <tr className="border-b text-left text-muted-foreground">
                    <th className="py-2 pr-3 font-medium">时间</th>
                    <th className="py-2 pr-3 font-medium">动作</th>
                    <th className="py-2 pr-3 font-medium">标的</th>
                    <th className="py-2 pr-3 font-medium">方向</th>
                    <th className="py-2 pr-3 font-medium">数量</th>
                    <th className="py-2 pr-3 font-medium">杠杆</th>
                    <th className="py-2 pr-3 font-medium">TP / SL</th>
                    <th className="py-2 pr-3 font-medium">状态</th>
                    <th className="py-2 font-medium">单号 / 错误</th>
                  </tr>
                </thead>
                <tbody>
                  {data!.actions.map((a: TradeAction) => (
                    <tr key={a.id} className="border-b last:border-0">
                      <td className="py-2 pr-3 text-xs text-muted-foreground">
                        {a.created_at ? new Date(a.created_at).toLocaleString("zh-CN") : "—"}
                      </td>
                      <td className="py-2 pr-3 text-xs font-medium">
                        {a.action === "open"
                          ? a.position_effect === "scale_in"
                            ? "加仓"
                            : a.position_effect === "reduce"
                              ? "减仓"
                              : a.position_effect === "reverse"
                                ? "反手"
                                : "开仓"
                          : a.action === "close"
                            ? "平仓"
                            : "调杠杆"}
                      </td>
                      <td className="py-2 pr-3 font-mono text-xs">{a.inst_id}</td>
                      <td className="py-2 pr-3 text-xs">
                        {a.pos_side === "short" ? "空" : a.pos_side === "long" ? "多" : "—"}
                        {a.side ? ` / ${a.side === "buy" ? "买" : "卖"}` : ""}
                      </td>
                      <td className="py-2 pr-3 font-mono text-xs">{a.qty || "—"}</td>
                      <td className="py-2 pr-3 font-mono text-xs">{a.leverage ? `${a.leverage}x` : "—"}</td>
                      <td className="py-2 pr-3 font-mono text-xs text-muted-foreground">
                        {a.tp_price || "—"} / {a.sl_price || "—"}
                      </td>
                      <td
                        className={`py-2 pr-3 text-xs ${
                          a.state === "failed" ? "text-amber-600" : "text-muted-foreground"
                        }`}
                      >
                        {a.state === "submitted" ? "已提交" : a.state === "filled" ? "已成交" : "失败"}
                      </td>
                      <td className="py-2 font-mono text-[10px] text-muted-foreground">
                        {a.error ? <span className="text-amber-600">{a.error}</span> : a.channel_order_id || "—"}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </CardContent>
      </Card>

      {/* ---- 实盘成交明细 ---- */}
      <Card>
        <CardHeader>
          <CardTitle className="text-sm">实盘成交明细</CardTitle>
          <CardDescription>最近 1000 笔，按时间升序。</CardDescription>
        </CardHeader>
        <CardContent>
          {liveCount === 0 ? (
            <p className="text-muted-foreground text-sm">
              {"本任务还没有任何成交。"}
            </p>
          ) : (
            <div className="max-h-[50vh] overflow-auto">
              <table className="w-full text-sm">
                <thead className="sticky top-0 bg-background">
                  <tr className="border-b text-left text-muted-foreground">
                    <th className="py-2 pr-3 font-medium">时间</th>
                    <th className="py-2 pr-3 font-medium">标的</th>
                    <th className="py-2 pr-3 font-medium">方向</th>
                    <th className="py-2 pr-3 font-medium">价格</th>
                    <th className="py-2 pr-3 font-medium">数量</th>
                    <th className="py-2 pr-3 font-medium">手续费</th>
                    <th className="py-2 pr-3 font-medium">订单号</th>
                    <th className="py-2 font-medium">成交号</th>
                  </tr>
                </thead>
                <tbody>
                  {liveFills.map((f) => (
                    <tr key={f.fill_id} className="border-b last:border-0">
                      <td className="py-2 pr-3 text-xs text-muted-foreground">
                        {new Date(f.filled_at).toLocaleString("zh-CN")}
                      </td>
                      <td className="py-2 pr-3 font-mono text-xs">{f.inst_id}</td>
                      <td
                        className={`py-2 pr-3 text-xs font-medium ${f.side === "buy" ? "text-red-500" : "text-green-600"}`}
                      >
                        {f.side === "buy" ? "买入" : "卖出"}
                        {f.pos_side && f.pos_side !== "net" ? `/${f.pos_side === "long" ? "多" : "空"}` : ""}
                      </td>
                      <td className="py-2 pr-3 font-mono text-xs">{fmtPlain(f.fill_px, 4)}</td>
                      <td className="py-2 pr-3 font-mono text-xs">{f.fill_sz}</td>
                      <td className="py-2 pr-3 font-mono text-xs text-muted-foreground">
                        {fmtPlain(f.fee, 6)} {f.fee_ccy}
                      </td>
                      <td className="py-2 pr-3 font-mono text-[10px] text-muted-foreground">{f.cl_ord_id || "—"}</td>
                      <td className="py-2 font-mono text-[10px] text-muted-foreground">{f.fill_id}</td>
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
