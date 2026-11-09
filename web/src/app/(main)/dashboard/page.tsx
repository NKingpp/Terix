"use client";

import * as React from "react";

import Link from "next/link";

import {
  ActivityIcon,
  ArrowUpRightIcon,
  BarChart3Icon,
  ClockIcon,
  CoinsIcon,
  LayersIcon,
  ShieldCheckIcon,
  TargetIcon,
  ZapIcon,
} from "lucide-react";
import { Bar, BarChart, CartesianGrid, Tooltip as RechartsTooltip, XAxis, YAxis } from "recharts";

import { StatusBadge } from "@/components/status-badge";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { type ChartConfig, ChartContainer, ChartTooltip, ChartTooltipContent } from "@/components/ui/chart";
import { Progress } from "@/components/ui/progress";
import { api } from "@/lib/api";
import type {
  Activity,
  Agent,
  ConvTokenSummary,
  DashboardSummary,
  InterceptPending,
  LLMProfile,
  MCPServer,
  Settings,
  SkillItem,
  Stats,
  Task,
  TokenTotal,
  Tool,
  UsageStats,
} from "@/lib/types";
import { cn } from "@/lib/utils";

// ── chart constants ───────────────────────────────────────────────────────────

const dailyTrendConfig = {
  input: { label: "输入", color: "hsl(217 91% 60%)" },
  output: { label: "输出", color: "hsl(263 70% 60%)" },
  cacheRead: { label: "缓存读", color: "hsl(160 60% 45%)" },
} satisfies ChartConfig;

// ── helpers ──────────────────────────────────────────────────────────────────

function fmtRel(ts?: string | number): string {
  if (!ts) return "—";
  const ms = Date.now() - (typeof ts === "number" ? ts * 1000 : Date.parse(ts as string));
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s 前`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m 前`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h 前`;
  return `${Math.floor(h / 24)}d 前`;
}

function fmtTokens(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + "M";
  if (n >= 1000) return (n / 1000).toFixed(1) + "k";
  return String(n);
}

function statusColor(code: number): string {
  if (code < 300) return "text-emerald-500";
  if (code < 400) return "text-blue-400";
  if (code < 500) return "text-amber-400";
  return "text-red-400";
}

function statusBg(code: number): string {
  if (code < 300) return "bg-emerald-500";
  if (code < 400) return "bg-blue-400";
  if (code < 500) return "bg-amber-400";
  return "bg-red-400";
}

// ── sub-components ────────────────────────────────────────────────────────────

function LiveDot({ className }: { className?: string }) {
  return <span className={cn("inline-block size-1.5 shrink-0 animate-pulse rounded-full bg-blue-400", className)} />;
}

// EXEC_MODE_LABEL 把后端的执行模式映射成中文。
//
// ⚠️ 值域来自 db/exec_mode.go 的常量。后端加新模式时，这里会退化成原样显示
// （不是空白）—— 那比「显示空白让人以为坏了」好。
const EXEC_MODE_LABEL: Record<string, string> = {
  paper: "纸面",
  live_whitelist: "实盘白名单",
  live_manual: "实盘手动",
  live_auto: "实盘自动",
  frozen: "已冻结",
};

// RATING_ORDER 是评级从强到弱的顺序。
//
// ⚠️ 值域来自数据库约束 ratings_rating_check 的 6 个值：
//   CHECK (rating = ANY (ARRAY['buy','overweight','hold','underweight','sell','review']))
// 加新评级值时，约束会先变 —— 这份清单要跟随。
// 不要凭记忆写（最初的设计就漏了 overweight）。
const RATING_ORDER = ["buy", "overweight", "hold", "underweight", "sell", "review"] as const;

const RATING_LABEL: Record<string, string> = {
  buy: "买入",
  overweight: "超配",
  hold: "持有",
  underweight: "低配",
  sell: "卖出",
  // ⚠️ review 不是「看多/看空」的结论 —— 它是「数据不足，无法给结论」。
  // 这一点在卡片底部用一行「有效结论 N/total」说明，不放在标签里
  // （标签加后缀会把 6 项条形的宽度撑开，而只有 review 需要）。
  review: "复核",
};

// 条形颜色：越强越绿，越弱越红。review 用中性色（它不是一个「看多/看空」结论）。
const RATING_BAR: Record<string, string> = {
  buy: "bg-emerald-500",
  overweight: "bg-emerald-400/70",
  hold: "bg-slate-400",
  underweight: "bg-amber-400/70",
  sell: "bg-rose-500",
  review: "bg-violet-400",
};

// ---------------------------------------------------------------------
// 盈亏展示的小工具
// ---------------------------------------------------------------------

/**
 * formatPnl 把 decimal 字符串格式化成带符号的金额。
 *
 * ⚠️ 正数要显式加 "+" —— 「1200」看不出是盈利，而「+1200」一眼就懂。
 * 负数保留系统的 "−"，0 不加符号（打平不需要方向）。
 *
 * 保留两位小数：盈亏是给人看的金额，不是记账凭证（记账用精确 decimal）。
 */
function formatPnl(v: string | null): string {
  if (v === null) return "—";
  const n = Number(v);
  if (!Number.isFinite(n)) return "—";
  const body = Math.abs(n).toLocaleString("zh-CN", {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  });
  if (n > 0) return `+${body}`;
  if (n < 0) return `−${body}`;
  return body;
}

/**
 * pnlColor 按盈亏方向给颜色。
 *
 * # 为什么用中国习惯（红涨绿跌）
 *
 * 本项目面向 A 股用户，红色=盈利、绿色=亏损。这与欧美相反 ——
 * 若这里用绿涨红跌，A 股用户第一眼会读反，而颜色是最快的第一信号。
 *
 * 0 与无法计算都用中性色：它们没有方向。
 */
function pnlColor(v: string | null): string {
  if (v === null) return "text-muted-foreground";
  const n = Number(v);
  if (!Number.isFinite(n) || n === 0) return "text-muted-foreground";
  return n > 0 ? "text-rose-500" : "text-emerald-500";
}

/**
 * PnlValue 渲染卡片主数字（浮动盈亏）。
 *
 * # 为什么 null 必须显示「—」而不是 0
 *
 * null 表示「算不出来」（没有现价）。显示 0 会被读成「有持仓但刚好打平」——
 * 那是把「未知」伪装成「已知」，比不显示更有害。
 */
function PnlValue({ value }: { value: string | null }) {
  if (value === null) {
    return <span className="text-muted-foreground text-base font-normal">—</span>;
  }
  return <span className={pnlColor(value)}>{formatPnl(value)}</span>;
}

function SectionTitle({
  icon: Icon,
  children,
  sub,
}: {
  icon: React.ElementType;
  children: React.ReactNode;
  sub?: React.ReactNode;
}) {
  return (
    <div className="mb-3 flex items-center justify-between">
      <div className="flex items-center gap-1.5 text-xs font-semibold">
        <Icon className="size-3.5 text-muted-foreground" />
        {children}
      </div>
      {sub && <span className="text-[10px] text-muted-foreground">{sub}</span>}
    </div>
  );
}

// ── page ─────────────────────────────────────────────────────────────────────

export default function DashboardPage() {
  // data state
  const [tasks, setTasks] = React.useState<Task[]>([]);
  // dashboard 的金融指标 —— 单一聚合接口，保证「同一时刻的快照」。
  const [summary, setSummary] = React.useState<DashboardSummary | null>(null);
  const [stats, setStats] = React.useState<Stats | null>(null);
  const [settings, setSettings] = React.useState<Settings | null>(null);
  const [pending, setPending] = React.useState<InterceptPending[]>([]);
  const [activity, setActivity] = React.useState<Activity[]>([]);
  const [tokens, setTokens] = React.useState<TokenTotal | null>(null);
  const [convTokens, setConvTokens] = React.useState<ConvTokenSummary[]>([]);
  const [usageStats, setUsageStats] = React.useState<UsageStats | null>(null);
  const [agents, setAgents] = React.useState<Agent[]>([]);
  const [mcpServers, setMcpServers] = React.useState<MCPServer[]>([]);
  const [skills, setSkills] = React.useState<SkillItem[]>([]);
  const [tools, setTools] = React.useState<Tool[]>([]);
  const [llmProfiles, setLLMProfiles] = React.useState<LLMProfile[]>([]);

  // The task list is the most expensive dashboard source. Poll it independently
  // so a large history cannot hold back every other dashboard panel.
  React.useEffect(() => {
    let alive = true;
    let loading = false;
    let signature = "";
    const load = async () => {
      if (loading) return;
      loading = true;
      try {
        const tr = await api.tasks();
        if (!alive) return;
        const nextSignature = JSON.stringify(tr.tasks);
        if (nextSignature !== signature) {
          signature = nextSignature;
          setTasks(tr.tasks);
        }
      } catch {
        /* transient errors — next poll retries */
      } finally {
        loading = false;
      }
    };
    void load();
    const t = setInterval(load, 5000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, []);

  // The remaining fast sources stay batched so one poll causes a single render.
  React.useEffect(() => {
    let alive = true;
    let loading = false;
    const load = async () => {
      if (loading) return;
      loading = true;
      try {
        const [stats, settings, pending, activity, tokens, conversationTokens, dash] =
          await Promise.all([
            api.stats(),
            api.settings(),
            api.interceptPending(),
            api.activity(undefined, { limit: 30 }),
            api.tokenStats(),
            api.conversationTokens(),
            api.dashboardSummary(),
          ]);
        if (!alive) return;
        setSummary(dash);
        setStats(stats);
        setSettings(settings);
        setPending(pending);
        setActivity(activity.items);
        setTokens(tokens.total ?? null);
        setConvTokens(conversationTokens);
      } catch {
        // Preserve stale data and retry on the next interval.
      } finally {
        loading = false;
      }
    };
    void load();
    const timer = setInterval(load, 5000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, []);

  // slow poll: assets, system-static (every 15s)
  React.useEffect(() => {
    let alive = true;
    const load = async () => {
      try {
        const [agentList, mcpList, skillList, toolList, profileList, usage] = await Promise.all([
          api.agents(),
          api.mcpServers(),
          api.skills(),
          api.tools(),
          api.llmProfiles(),
          api.usageStats(365),
        ]);
        if (!alive) return;
        setAgents(agentList);
        setMcpServers(mcpList);
        setSkills(skillList);
        setTools(toolList);
        setLLMProfiles(profileList);
        setUsageStats(usage);
      } catch {
        /* transient errors */
      }
    };
    void load();
    const t = setInterval(load, 15000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, []);

  // ── derived ───────────────────────────────────────────────────────────────

  const tasksByStatus = React.useMemo(() => {
    const m: Record<string, number> = {};
    for (const t of tasks) m[t.status] = (m[t.status] ?? 0) + 1;
    return m;
  }, [tasks]);


  const sortedTasks = React.useMemo(
    () =>
      [...tasks]
        .sort((a, b) => (b.last_activity_unix ?? b.created_unix ?? 0) - (a.last_activity_unix ?? a.created_unix ?? 0))
        .slice(0, 5),
    [tasks],
  );

  const recentActivity = React.useMemo(
    () =>
      [...activity]
        .filter((a) => a.kind !== "usage")
        .sort((a, b) => b.seq - a.seq)
        .slice(0, 6),
    [activity],
  );

  // system
  const activeProfile = llmProfiles.find((p) => p.is_default);
  const enabledTools = tools.filter((t) => t.enabled);
  const pendingCount = pending.length;

  // ── token stats per LLM profile ──────────────────────────────────────────
  // tasks whose llm_profile_id is null/undefined used the active default profile
  const defaultProfileId = activeProfile ? Number(activeProfile.id) : null;

  // 数据源开关：旧版 = activity（task.tokens + 会话），新版 = llm_usage 计量账本。
  const [tokenVersion, setTokenVersion] = React.useState<"old" | "new">("old");
  // selected profile tab: "all" = 全部; number = specific profile id
  const [tokenTab, setTokenTab] = React.useState<number | null | "all">("all");
  // day range for the daily bar chart
  const [tokenDays, setTokenDays] = React.useState<7 | 30 | 90 | 180 | 365>(30);

  // profile 名 → id，用于把 llm_usage 的 profile_name 映射到现有 profile 分栏。
  const profileIdByName = React.useMemo(() => {
    const m = new Map<string, number>();
    for (const p of llmProfiles) m.set(p.name, Number(p.id));
    return m;
  }, [llmProfiles]);

  type Bucket = { input: number; output: number; cacheRead: number; cacheWrite: number; taskCount: number };

  // 旧版：按 profile 归桶（来自 activity 的 task.tokens + 会话用量）。
  const tokenByProfileOld = React.useMemo<Map<number | null, Bucket>>(() => {
    const m = new Map<number | null, Bucket>();
    const fold = (key: number | null, inp: number, out: number, cr: number, cw: number, addTask: boolean) => {
      const prev = m.get(key) ?? { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, taskCount: 0 };
      m.set(key, {
        input: prev.input + inp,
        output: prev.output + out,
        cacheRead: prev.cacheRead + cr,
        cacheWrite: prev.cacheWrite + cw,
        taskCount: prev.taskCount + (addTask ? 1 : 0),
      });
    };
    for (const t of tasks) {
      fold(
        t.llm_profile_id ?? defaultProfileId,
        t.tokens?.input_tokens ?? 0,
        t.tokens?.output_tokens ?? 0,
        t.tokens?.cache_read_tokens ?? 0,
        t.tokens?.cache_write_tokens ?? 0,
        true,
      );
    }
    for (const c of convTokens) {
      fold(
        c.llm_profile_id ?? defaultProfileId,
        c.input_tokens,
        c.output_tokens,
        c.cache_read_tokens,
        c.cache_write_tokens,
        false,
      );
    }
    return m;
  }, [tasks, convTokens, defaultProfileId]);

  // 新版：按 profile 归桶（来自 llm_usage 全局聚合，逐次调用精确）。
  const tokenByProfileNew = React.useMemo<Map<number | null, Bucket>>(() => {
    const m = new Map<number | null, Bucket>();
    for (const p of usageStats?.by_profile ?? []) {
      // 未匹配到现有 profile（改名/删除/空名）→ 落到默认桶，仍计入「全部」。
      const key = profileIdByName.get(p.profile_name) ?? defaultProfileId;
      const prev = m.get(key) ?? { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, taskCount: 0 };
      m.set(key, {
        input: prev.input + p.input_tokens,
        output: prev.output + p.output_tokens,
        cacheRead: prev.cacheRead + p.cache_read_tokens,
        cacheWrite: prev.cacheWrite + p.cache_write_tokens,
        taskCount: prev.taskCount + p.tasks,
      });
    }
    return m;
  }, [usageStats, profileIdByName, defaultProfileId]);

  const tokenByProfile = tokenVersion === "new" ? tokenByProfileNew : tokenByProfileOld;

  const displayedTokens = React.useMemo(() => {
    if (tokenTab === "all") {
      let input = 0,
        output = 0,
        cacheRead = 0,
        cacheWrite = 0,
        taskCount = 0;
      for (const v of tokenByProfile.values()) {
        input += v.input;
        output += v.output;
        cacheRead += v.cacheRead;
        cacheWrite += v.cacheWrite;
        taskCount += v.taskCount;
      }
      return { input, output, cacheRead, cacheWrite, taskCount };
    }
    const v = tokenByProfile.get(tokenTab as number | null);
    return v
      ? { input: v.input, output: v.output, cacheRead: v.cacheRead, cacheWrite: v.cacheWrite, taskCount: v.taskCount }
      : { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, taskCount: 0 };
  }, [tokenTab, tokenByProfile]);

  // 旧版每日：把任务/会话的总量按其创建日期归桶（近似，非真实每日消耗）。
  const dailyTokenDataOld = React.useMemo(() => {
    const now = new Date();
    now.setDate(now.getDate() - tokenDays);
    const cutoff = now.toISOString().slice(0, 10);
    const m = new Map<string, { input: number; output: number; cacheRead: number }>();
    const fold = (date: string, inp: number, out: number, cr: number) => {
      if (date < cutoff) return;
      const prev = m.get(date) ?? { input: 0, output: 0, cacheRead: 0 };
      m.set(date, { input: prev.input + inp, output: prev.output + out, cacheRead: prev.cacheRead + cr });
    };
    for (const t of tasks) {
      if (tokenTab !== "all" && (t.llm_profile_id ?? defaultProfileId) !== tokenTab) continue;
      fold(
        t.created_at.slice(0, 10),
        t.tokens?.input_tokens ?? 0,
        t.tokens?.output_tokens ?? 0,
        t.tokens?.cache_read_tokens ?? 0,
      );
    }
    for (const c of convTokens) {
      if (tokenTab !== "all" && (c.llm_profile_id ?? defaultProfileId) !== tokenTab) continue;
      fold(c.created_at.slice(0, 10), c.input_tokens, c.output_tokens, c.cache_read_tokens);
    }
    return [...m.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([date, v]) => ({ date: date.slice(5), ...v }));
  }, [tasks, convTokens, tokenDays, tokenTab, defaultProfileId]);

  // 新版每日：来自 llm_usage 的真实每日消耗（ts 是实际调用时刻）。
  const dailyTokenDataNew = React.useMemo(() => {
    const now = new Date();
    now.setDate(now.getDate() - tokenDays);
    const cutoff = now.toISOString().slice(0, 10);
    const m = new Map<string, { input: number; output: number; cacheRead: number }>();
    for (const d of usageStats?.daily ?? []) {
      if (d.date < cutoff) continue;
      if (tokenTab !== "all" && (profileIdByName.get(d.profile_name) ?? defaultProfileId) !== tokenTab) continue;
      const prev = m.get(d.date) ?? { input: 0, output: 0, cacheRead: 0 };
      m.set(d.date, {
        input: prev.input + d.input_tokens,
        output: prev.output + d.output_tokens,
        cacheRead: prev.cacheRead + d.cache_read_tokens,
      });
    }
    return [...m.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([date, v]) => ({ date: date.slice(5), ...v }));
  }, [usageStats, tokenDays, tokenTab, profileIdByName, defaultProfileId]);

  const dailyTokenData = tokenVersion === "new" ? dailyTokenDataNew : dailyTokenDataOld;

  // activity kind label
  function kindLabel(a: Activity): string {
    if (a.kind === "tool_use") return a.tool ?? "tool_use";
    if (a.kind === "tool_result") return "tool_result";
    return a.kind;
  }

  function workerColor(w: string): string {
    if (w === "planner") return "text-violet-400";
    if (w === "mainagent") return "text-cyan-400";
    return "text-blue-400";
  }

  function workerBg(w: string): string {
    if (w === "planner") return "bg-violet-500/10 text-violet-400 border-violet-500/20";
    if (w === "mainagent") return "bg-cyan-500/10 text-cyan-400 border-cyan-500/20";
    return "bg-blue-500/10 text-blue-400 border-blue-500/20";
  }

  // ── render ────────────────────────────────────────────────────────────────

  return (
    <div className="flex flex-1 flex-col gap-4 pb-6">
      {/* ── Header ── */}
      <div>
        <div>
          <h1 className="text-lg font-semibold tracking-tight">总览</h1>
          <p className="text-xs text-muted-foreground">系统全局状态 · 实时刷新</p>
        </div>
      </div>

      {/* ── Row 1: 4 张金融卡 ──
          这是产品的核心信息：执行状态、待决策、评级、纸面盈亏。
          运维指标（Token 用量等）在 Row 2 —— 它们是成本信息，用户偶尔看。 */}
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-2 lg:grid-cols-4">
        {/* 执行模式：点击进 /exec（那里有完整的管理界面） */}
        <Link href="/exec" className="block">
          <Card className="hover:bg-muted/40 h-full gap-1 transition">
            <CardHeader className="pb-0">
              <div className="flex items-center gap-1.5 text-[10px] text-muted-foreground">
                <ShieldCheckIcon className="size-3" /> 执行模式
                <ArrowUpRightIcon className="ml-auto size-2.5 opacity-40" />
              </div>
              <div className="flex items-baseline gap-1.5">
                <span className="text-2xl font-semibold tabular-nums">
                  {summary ? EXEC_MODE_LABEL[summary.exec.mode] ?? summary.exec.mode : "—"}
                </span>
              </div>
            </CardHeader>
            <CardContent className="flex flex-wrap gap-x-2.5 gap-y-0.5 text-[10px] text-muted-foreground">
              {summary ? (
                <>
                  <span>
                    账户 {summary.exec.accounts.enabled}/{summary.exec.accounts.total}
                  </span>
                  {summary.exec.accounts.blocked > 0 && (
                    <span className="text-rose-400">冻结 {summary.exec.accounts.blocked}</span>
                  )}
                  <span className={summary.exec.frozen ? "text-rose-400" : "text-emerald-400"}>
                    {summary.exec.frozen ? "已冻结" : "正常运行"}
                  </span>
                </>
              ) : (
                <span>加载中…</span>
              )}
            </CardContent>
          </Card>
        </Link>

        {/* 待决策草案：只统计「还没终结」的草案 */}
        <Card className="gap-1">
          <CardHeader className="pb-0">
            <div className="flex items-center gap-1.5 text-[10px] text-muted-foreground">
              <LayersIcon className="size-3" /> 待决策草案
            </div>
            <div className="text-2xl font-semibold tabular-nums">{summary?.drafts.total ?? "—"}</div>
          </CardHeader>
          <CardContent className="flex flex-wrap gap-x-2.5 gap-y-0.5 text-[10px]">
            {summary && summary.drafts.total > 0 ? (
              <>
                {/* 只显示非 0 的分类 —— 一排 0 会让人以为有问题 */}
                {summary.drafts.by_verdict.pass > 0 && (
                  <span className="text-emerald-400">pass {summary.drafts.by_verdict.pass}</span>
                )}
                {summary.drafts.by_verdict.ask > 0 && (
                  <span className="text-amber-400">ask {summary.drafts.by_verdict.ask}</span>
                )}
                {summary.drafts.by_verdict.deny > 0 && (
                  <span className="text-rose-400">deny {summary.drafts.by_verdict.deny}</span>
                )}
                {summary.drafts.by_verdict.none > 0 && (
                  <span className="text-muted-foreground">未判定 {summary.drafts.by_verdict.none}</span>
                )}
                {/* other 只在 sum != total 时出现 —— 它的存在就是「数据有脏值」的信号 */}
                {(summary.drafts.by_verdict.other ?? 0) > 0 && (
                  <span className="text-rose-500">脏值 {summary.drafts.by_verdict.other}</span>
                )}
              </>
            ) : (
              <span className="text-muted-foreground">无待决策草案</span>
            )}
          </CardContent>
        </Card>

        {/* 评级分布：6 项横向条形 */}
        <Card className="gap-1">
          <CardHeader className="pb-0">
            <div className="flex items-center gap-1.5 text-[10px] text-muted-foreground">
              <BarChart3Icon className="size-3" /> 评级分布
            </div>
            <div className="text-2xl font-semibold tabular-nums">{summary?.ratings.total ?? "—"}</div>
          </CardHeader>
          <CardContent className="text-[10px]">
            {summary && summary.ratings.total > 0 ? (
              <div className="flex flex-col gap-0.5">
                {/* ⚠️ 百分比分母必须排除 review。
                    2026-11-04 修正：原来用 total（含 review）作分母，
                    review 一多，buy/sell 的条形全被压短 ——
                    视觉上像「多空双方都很弱」，而实际是「大部分数据不足，
                    有效样本本来就少」。分母改用有效结论数才反映真实占比。
                    total=全为 review 时 validTotal=0，此时改用 1 避免除零
                    （条形宽度全为 0，符合「无有效结论」的事实）。 */}
                {(() => {
                  const validTotal = summary.ratings.total - (summary.ratings.by_rating.review ?? 0);
                  const denom = validTotal > 0 ? validTotal : 1;
                  return RATING_ORDER.filter((r) => r !== "review").map((r) => {
                    const n = summary.ratings.by_rating[r] ?? 0;
                    if (n === 0) return null;
                    return (
                      <div key={r} className="flex items-center gap-1.5">
                        <span className="w-16 shrink-0 truncate">
                          {RATING_LABEL[r]}
                        </span>
                        <div className="h-1.5 flex-1 overflow-hidden rounded-sm bg-muted">
                          <div
                            className={cn("h-full rounded-sm", RATING_BAR[r])}
                            style={{ width: `${Math.round((n / denom) * 100)}%` }}
                          />
                        </div>
                        <span className="w-4 shrink-0 text-right tabular-nums text-muted-foreground">{n}</span>
                      </div>
                    );
                  });
                })()}
                {/* review 单独一行，用中性样式 —— 它不是看多/看空的第六档。
                    放在条形区下方、与「有效结论」说明连成一组，
                    视觉上明确「这是样本质量，不是观点分布」。 */}
                {(summary.ratings.by_rating.review ?? 0) > 0 && (
                  <div className="flex items-center gap-1.5 text-muted-foreground">
                    <span className="w-16 shrink-0 truncate">{RATING_LABEL.review}</span>
                    <div className="h-1.5 flex-1 overflow-hidden rounded-sm bg-muted">
                      <div
                        className="h-full rounded-sm bg-muted-foreground/40"
                        style={{
                          width: `${Math.round(
                            ((summary.ratings.by_rating.review ?? 0) / summary.ratings.total) * 100,
                          )}%`,
                        }}
                      />
                    </div>
                    <span className="w-4 shrink-0 text-right tabular-nums">
                      {summary.ratings.by_rating.review}
                    </span>
                  </div>
                )}
              </div>
            ) : (
              // ⚠️ 空态显示后端返回的 note（它说明「为什么空」），
              // 而不是自己编一句 —— 因为「没分析」和「分析过但没产出」只有后端分得清。
              <span className="text-muted-foreground">{summary?.ratings.note ?? "加载中…"}</span>
            )}

            {/* 「有效结论」说明：只在有 review 时显示。
                ⚠️ review 是「数据不足，无法给结论」，不是第六种看多/看空观点。
                如果 review 占多数，只看条形会让人以为「系统倾向复核」，
                而实际是「大部分分析数据不足」。这行把那个区别说清。 */}
            {summary && summary.ratings.total > 0 &&
              (summary.ratings.by_rating.review ?? 0) > 0 && (
                <p className="mt-1.5 border-t pt-1.5 leading-snug text-muted-foreground">
                  其中 {summary.ratings.by_rating.review} 条为「待复核」（数据不足，不计入多空判断）·
                  有效结论 {summary.ratings.total - summary.ratings.by_rating.review}/{summary.ratings.total}
                </p>
              )}
          </CardContent>
        </Card>

        {/* 纸面盈亏：已实现 / 浮动分开呈现。
            ⚠️ 已实现（落袋）与浮动（账面）的决策含义完全不同 ——
            合并成「总盈亏」会让用户无法判断那笔钱会不会吐回去。 */}
        <Card className="gap-1">
          <CardHeader className="pb-0">
            <div className="flex items-center gap-1.5 text-[10px] text-muted-foreground">
              <CoinsIcon className="size-3" /> 纸面盈亏
            </div>
            <div className="text-2xl font-semibold tabular-nums">
              {!summary ? (
                "—"
              ) : summary.pnl.paper_orders === 0 ? (
                // 「无成交」与「¥0.00」是两件事：后者会被读成「有成交但打平」
                <span className="text-muted-foreground text-base font-normal">无成交</span>
              ) : (
                <PnlValue value={summary.pnl.unrealized_pnl} />
              )}
            </div>
          </CardHeader>
          <CardContent className="flex flex-col gap-0.5 text-[10px]">
            {summary && summary.pnl.paper_orders > 0 && (
              <>
                <div className="flex justify-between gap-2">
                  <span className="text-muted-foreground">已实现</span>
                  <span className={cn("tabular-nums", pnlColor(summary.pnl.realized_pnl))}>
                    {formatPnl(summary.pnl.realized_pnl)}
                  </span>
                </div>
                <div className="flex justify-between gap-2">
                  <span className="text-muted-foreground">持仓</span>
                  <span className="tabular-nums">{summary.pnl.positions}</span>
                </div>
              </>
            )}
            <span className="text-muted-foreground leading-snug">
              {summary
                ? summary.pnl.note ||
                  (summary.pnl.paper_orders > 0 ? "浮动盈亏按最新收盘价计算" : "")
                : "加载中…"}
            </span>
          </CardContent>
        </Card>
      </div>

      {/* 全空引导：后端说「为什么空」（ratings.note），这里说「怎么办」。
          ⚠️ 分工：note 是事实（只有后端查得出），这句是行动（只有前端知道跳哪）。 */}
      {summary && summary.drafts.total === 0 && summary.ratings.total === 0 && (
        <div className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-dashed px-4 py-3">
          <p className="text-xs text-muted-foreground">
            {summary.ratings.note || "还没有分析记录"}
          </p>
          <Link
            href="/function/tasks"
            className="text-xs font-medium text-blue-400 hover:underline"
          >
            去创建任务 →
          </Link>
        </div>
      )}

      {/* ── Row 2a: 运维卡片 ──
          活跃任务从原 Row1 移来 —— 它是通用指标，但优先级低于金融指标。 */}
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-4">
        {/* 活跃任务 */}
        <Card className="gap-1">
          <CardHeader className="pb-0">
            <div className="flex items-center gap-1.5 text-[10px] text-muted-foreground">
              <TargetIcon className="size-3" /> 活跃任务
            </div>
            <div className="flex items-baseline gap-1.5">
              <span className="text-2xl font-semibold tabular-nums">{tasksByStatus.running ?? 0}</span>
              <span className="text-xs text-muted-foreground">/ {tasks.length}</span>
            </div>
          </CardHeader>
          <CardContent className="flex flex-wrap gap-x-2.5 gap-y-0.5 text-[10px]">
            {(tasksByStatus.running ?? 0) > 0 && <span className="text-blue-400">探索 {tasksByStatus.running}</span>}
            {(tasksByStatus.paused ?? 0) > 0 && <span className="text-amber-400">暂停 {tasksByStatus.paused}</span>}
            {(tasksByStatus.done ?? 0) > 0 && <span className="text-emerald-400">完成 {tasksByStatus.done}</span>}
            {tasks.length === 0 && <span className="text-muted-foreground">暂无任务</span>}
          </CardContent>
        </Card>

      </div>

      {/* ── Row 2: LLM Token 消耗 ── */}
      <Card className="p-4">
        {/* Header */}
        <div className="mb-4 flex flex-wrap items-center justify-between gap-2">
          <div className="flex items-center gap-1.5 text-xs font-semibold">
            <ZapIcon className="size-3.5 text-muted-foreground" />
            LLM Token 消耗
            {/* 数据源开关：旧版=activity 统计（含历史任务），新版=llm_usage 计量账本（更准，仅覆盖启用后） */}
            <div className="ml-1 flex gap-0.5 rounded-md border bg-muted/30 p-0.5">
              {(
                [
                  { v: "old", label: "旧版" },
                  { v: "new", label: "新版" },
                ] as const
              ).map(({ v, label }) => (
                <button
                  type="button"
                  key={v}
                  onClick={() => setTokenVersion(v)}
                  title={
                    v === "new"
                      ? "新版：来自 llm_usage 计量账本，逐次调用精确、含中断消耗；仅覆盖启用后的数据"
                      : "旧版：来自 activity 统计（含历史任务），中断消耗不计、无法精确到模型"
                  }
                  className={cn(
                    "rounded px-2 py-0.5 text-[9px] font-medium transition-colors",
                    tokenVersion === v
                      ? "bg-background text-foreground shadow-sm"
                      : "text-muted-foreground hover:text-foreground",
                  )}
                >
                  {label}
                </button>
              ))}
            </div>
            <span className="text-[10px] font-normal text-muted-foreground">
              {tokenVersion === "new" ? "llm_usage" : "activity"}
            </span>
          </div>
          {/* Profile tabs */}
          <div className="flex flex-wrap items-center gap-1">
            <button
              type="button"
              onClick={() => setTokenTab("all")}
              className={cn(
                "rounded-md px-2.5 py-1 text-[10px] font-medium transition-colors",
                tokenTab === "all"
                  ? "bg-foreground text-background"
                  : "bg-muted/30 text-muted-foreground hover:text-foreground",
              )}
            >
              全部
            </button>
            {llmProfiles.map((p) => {
              const key = Number(p.id);
              const hasData = tokenByProfile.has(key) || (p.is_default && tokenByProfile.has(defaultProfileId));
              const resolvedKey = tokenByProfile.has(key) ? key : p.is_default ? defaultProfileId : key;
              return (
                <button
                  type="button"
                  key={p.id}
                  onClick={() => setTokenTab(resolvedKey)}
                  className={cn(
                    "flex items-center gap-1 rounded-md px-2.5 py-1 text-[10px] font-medium transition-colors",
                    tokenTab === resolvedKey
                      ? "bg-foreground text-background"
                      : "bg-muted/30 text-muted-foreground hover:text-foreground",
                    !hasData && "opacity-40",
                  )}
                >
                  {p.name}
                  {p.is_default && (
                    <span
                      className={cn(
                        "rounded px-1 py-0 text-[8px]",
                        tokenTab === resolvedKey
                          ? "bg-background/20 text-background"
                          : "bg-emerald-500/20 text-emerald-400",
                      )}
                    >
                      默认
                    </span>
                  )}
                </button>
              );
            })}
          </div>
        </div>

        {/* Body: metrics left + bar chart right */}
        <div className="grid grid-cols-1 gap-6 lg:grid-cols-[220px_1fr]">
          {/* Left: key metrics */}
          <div className="flex flex-col gap-4">
            {/* Total */}
            <div>
              <div className="text-[10px] text-muted-foreground">合计 (输入+输出)</div>
              <div className="mt-0.5 text-3xl font-bold tabular-nums tracking-tight">
                {fmtTokens(displayedTokens.input + displayedTokens.output) || "—"}
              </div>
              <div className="mt-0.5 text-[10px] text-muted-foreground">{displayedTokens.taskCount} 个任务</div>
            </div>

            {/* Per-type bars */}
            <div className="space-y-3">
              {(() => {
                // input 已含缓存；拆成不重叠三段：未命中输入 + 缓存命中 + 输出 = 总量。
                const total = displayedTokens.input + displayedTokens.output;
                return [
                  {
                    label: "输入(未命中)",
                    value: displayedTokens.input - displayedTokens.cacheRead,
                    barColor: dailyTrendConfig.input.color!,
                    text: "text-blue-400",
                  },
                  {
                    label: "缓存命中",
                    value: displayedTokens.cacheRead,
                    barColor: dailyTrendConfig.cacheRead.color!,
                    text: "text-emerald-400",
                  },
                  {
                    label: "输出",
                    value: displayedTokens.output,
                    barColor: dailyTrendConfig.output.color!,
                    text: "text-violet-400",
                  },
                ].map(({ label, value, barColor, text }) => {
                  const pct = total > 0 ? (value / total) * 100 : 0;
                  return (
                    <div key={label}>
                      <div className="mb-1 flex items-center justify-between text-[10px]">
                        <span className="text-muted-foreground">{label}</span>
                        <span className={cn("font-mono font-semibold tabular-nums", text)}>{fmtTokens(value)}</span>
                      </div>
                      <div className="h-1.5 overflow-hidden rounded-full bg-muted">
                        <div
                          className="h-full rounded-full transition-all duration-500"
                          style={{ width: `${pct}%`, background: barColor }}
                        />
                      </div>
                    </div>
                  );
                });
              })()}
            </div>

            {/* Cache hit rate */}
            {(() => {
              // input 已含缓存 → 命中率 = 缓存命中 / 总输入。
              const denominator = displayedTokens.input;
              const hitPct = denominator > 0 ? Math.round((displayedTokens.cacheRead / denominator) * 100) : 0;
              return (
                <div className="flex items-center justify-between rounded-lg border bg-muted/20 px-3 py-2 text-[10px]">
                  <span className="text-muted-foreground">缓存命中率</span>
                  <span
                    className={cn("font-semibold tabular-nums", hitPct > 50 ? "text-emerald-400" : "text-amber-400")}
                  >
                    {hitPct}%
                  </span>
                </div>
              );
            })()}
          </div>

          {/* Right: daily bar chart */}
          <div className="flex min-h-0 flex-col">
            <div className="mb-2 flex items-center justify-between">
              <div className="flex gap-3 text-[9px] text-muted-foreground">
                {(["input", "output", "cacheRead"] as const).map((k) => (
                  <span key={k} className="flex items-center gap-1">
                    <span
                      className="inline-block size-2 rounded-sm"
                      style={{ background: dailyTrendConfig[k].color }}
                    />
                    {dailyTrendConfig[k].label}
                  </span>
                ))}
              </div>
              <div className="flex gap-0.5 rounded-md border bg-muted/30 p-0.5">
                {(
                  [
                    { days: 7, label: "7天" },
                    { days: 30, label: "30天" },
                    { days: 90, label: "3月" },
                    { days: 180, label: "6月" },
                    { days: 365, label: "一年" },
                  ] as const
                ).map(({ days, label }) => (
                  <button
                    type="button"
                    key={days}
                    onClick={() => setTokenDays(days)}
                    className={cn(
                      "rounded px-2.5 py-0.5 text-[9px] font-medium transition-colors",
                      tokenDays === days
                        ? "bg-background text-foreground shadow-sm"
                        : "text-muted-foreground hover:text-foreground",
                    )}
                  >
                    {label}
                  </button>
                ))}
              </div>
            </div>

            {dailyTokenData.length === 0 ? (
              <div
                className="flex flex-1 items-center justify-center rounded-lg border bg-muted/10 text-xs text-muted-foreground"
                style={{ minHeight: 180 }}
              >
                暂无数据
              </div>
            ) : (
              <ChartContainer config={dailyTrendConfig} className="h-[200px] w-full">
                <BarChart data={dailyTokenData} margin={{ top: 4, right: 4, bottom: 0, left: 0 }} maxBarSize={40}>
                  <CartesianGrid vertical={false} />
                  <XAxis
                    dataKey="date"
                    tickLine={false}
                    axisLine={false}
                    tick={{ fontSize: 10 }}
                    tickMargin={6}
                    interval="preserveStartEnd"
                  />
                  <YAxis
                    tickFormatter={(v) => fmtTokens(Number(v))}
                    tickLine={false}
                    axisLine={false}
                    tick={{ fontSize: 10 }}
                    width={44}
                  />
                  <ChartTooltip
                    cursor={false}
                    content={
                      <ChartTooltipContent
                        indicator="line"
                        formatter={(value, name) => (
                          <div className="flex w-full items-center justify-between gap-4">
                            <span>
                              {(dailyTrendConfig as Record<string, { label: string }>)[String(name)]?.label ??
                                String(name)}
                            </span>
                            <span className="font-mono font-semibold tabular-nums">{fmtTokens(Number(value))}</span>
                          </div>
                        )}
                      />
                    }
                  />
                  <Bar dataKey="input" stackId="1" fill="var(--color-input)" radius={[0, 0, 0, 0]} />
                  <Bar dataKey="output" stackId="1" fill="var(--color-output)" radius={[0, 0, 0, 0]} />
                  <Bar dataKey="cacheRead" stackId="1" fill="var(--color-cacheRead)" radius={[4, 4, 0, 0]} />
                </BarChart>
              </ChartContainer>
            )}
          </div>
        </div>
      </Card>

      {/* ── Row 3: 活动流 ── */}
      <div className="grid grid-cols-1 gap-3">
        {/* 活动流 */}
        <Card className="p-4">
          <div className="mb-3 flex items-center justify-between">
            <div className="flex items-center gap-1.5 text-xs font-semibold">
              <ActivityIcon className="size-3.5 text-muted-foreground" />
              活动流
            </div>
            <div className="flex items-center gap-2">
              <span className="text-[10px] text-muted-foreground">
                {activity.filter((a) => a.kind !== "usage").length} 条事件
              </span>
              <Link
                href="/function/tasks"
                className="flex items-center gap-0.5 text-[10px] text-muted-foreground hover:text-foreground"
              >
                查看任务 <ArrowUpRightIcon className="size-3" />
              </Link>
            </div>
          </div>

          <div className="mb-2.5 flex flex-wrap gap-1.5">
            {(["tool_use", "tool_result", "text", "thinking", "result"] as const).map((kind) => {
              const count = activity.filter((a) => a.kind === kind).length;
              if (count === 0) return null;
              return (
                <span key={kind} className="rounded border bg-muted/20 px-1.5 py-0.5 text-[9px] text-muted-foreground">
                  {kind} <strong className="text-foreground/70">{count}</strong>
                </span>
              );
            })}
          </div>

          <div className="divide-y">
            {recentActivity.length === 0 ? (
              <div className="py-4 text-center text-xs text-muted-foreground">暂无活动记录</div>
            ) : (
              recentActivity.map((a) => (
                <div key={a.seq} className="flex gap-2.5 py-2">
                  <span
                    className={cn(
                      "shrink-0 self-start rounded border px-1.5 py-0.5 font-mono text-[9px]",
                      workerBg(a.worker),
                    )}
                  >
                    {a.worker}
                  </span>
                  <div className="min-w-0 flex-1">
                    <div className="text-[11px] font-medium">{kindLabel(a)}</div>
                    <div className="mt-0.5 truncate text-[10px] text-muted-foreground">{a.summary}</div>
                  </div>
                  <div className="shrink-0 text-right">
                    <Badge variant="outline" className="px-1.5 py-0 text-[9px] text-muted-foreground">
                      {a.kind}
                    </Badge>
                    <div className="mt-0.5 text-[9px] tabular-nums text-muted-foreground">{fmtRel(a.ts)}</div>
                  </div>
                </div>
              ))
            )}
          </div>
        </Card>

      </div>

      {/* ── Row 4: 任务表格 ── */}
      <Card className="overflow-hidden p-0">
        <div className="flex items-center justify-between border-b px-4 py-3">
          <div className="flex items-center gap-1.5 text-xs font-semibold">
            <ClockIcon className="size-3.5 text-muted-foreground" />
            任务
          </div>
          <div className="flex items-center gap-2">
            <span className="text-[10px] text-muted-foreground">{tasks.length} 个任务</span>
            <Link
              href="/function/tasks"
              className="flex items-center gap-0.5 text-[10px] text-muted-foreground hover:text-foreground"
            >
              全部 <ArrowUpRightIcon className="size-3" />
            </Link>
          </div>
        </div>
        <table className="w-full border-collapse text-xs">
          <thead>
            <tr className="border-b">
              {["任务", "状态", "引擎", "目标进度", "在途", "最近活动"].map((h) => (
                <th
                  key={h}
                  className="px-4 py-2 text-left text-[9px] font-semibold uppercase tracking-widest text-muted-foreground first:pl-4"
                >
                  {h}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {sortedTasks.length === 0 ? (
              <tr>
                <td colSpan={6} className="px-4 py-6 text-center text-xs text-muted-foreground">
                  暂无任务
                </td>
              </tr>
            ) : (
              sortedTasks.map((t) => {
                const goalsPct = t.goals_total ? Math.round(((t.goals_met ?? 0) / t.goals_total) * 100) : null;
                return (
                  <tr key={t.id} className="border-b last:border-0 transition-colors hover:bg-muted/30">
                    <td className="max-w-xs px-4 py-3">
                      <Link href={`/function/tasks/detail?id=${t.id}`} className="group flex flex-col">
                        <span className="truncate font-medium group-hover:underline">{t.description}</span>
                        <span className="mt-0.5 truncate font-mono text-[10px] text-muted-foreground">{t.id}</span>
                      </Link>
                    </td>
                    <td className="px-4 py-3">
                      <StatusBadge domain="task" value={t.status} dot className="px-1.5 py-0 text-[10px]" />
                    </td>
                    <td className="px-4 py-3">
                      <StatusBadge
                        domain="engine"
                        value={t.engine_mode ?? "idle"}
                        className="px-1.5 py-0 text-[10px]"
                      />
                    </td>
                    <td className="px-4 py-3">
                      {goalsPct !== null ? (
                        <div className="flex items-center gap-2">
                          <Progress value={goalsPct} className="h-1 w-14" />
                          <span className="tabular-nums text-muted-foreground">
                            {t.goals_met ?? 0}/{t.goals_total}
                          </span>
                        </div>
                      ) : (
                        <span className="text-muted-foreground">—</span>
                      )}
                    </td>
                    <td className="px-4 py-3 tabular-nums">
                      {(t.in_flight ?? 0) > 0 ? (
                        <span className="font-semibold">{t.in_flight}</span>
                      ) : (
                        <span className="text-muted-foreground">—</span>
                      )}
                    </td>
                    <td className="px-4 py-3 tabular-nums text-muted-foreground">
                      {fmtRel(t.last_activity_unix ?? t.created_unix)}
                    </td>
                  </tr>
                );
              })
            )}
          </tbody>
        </table>
      </Card>

      {/* ── Row 5: 系统状态 & 待审批 ── */}
      <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
        {/* 系统状态 & 待审批 */}
        <Card className="p-4">
          <SectionTitle icon={ShieldCheckIcon}>系统状态</SectionTitle>

          <div className="flex flex-col gap-2">
            <div className="flex items-center justify-between rounded-lg border bg-muted/20 px-3 py-2">
              <div className="text-[10px] text-muted-foreground">LLM 配置</div>
              <Badge
                variant="outline"
                className={cn(
                  "text-[10px]",
                  stats?.llm_configured
                    ? "border-emerald-500/30 bg-emerald-500/10 text-emerald-400"
                    : "border-red-500/30 bg-red-500/10 text-red-400",
                )}
              >
                {stats?.llm_configured ? "已配置" : "未配置"}
              </Badge>
            </div>


            {activeProfile && (
              <div className="flex items-center justify-between rounded-lg border bg-muted/20 px-3 py-2">
                <div className="text-[10px] text-muted-foreground">激活模型</div>
                <span className="font-mono text-[10px]">{activeProfile.model}</span>
              </div>
            )}
          </div>

          {/* pending approvals
              ⚠️ 这里原本每一项都是指向审批页的 Link，
              但那个页面在第 1 批（2026-10-29）随渗透 UI 整目录删除 —— 点进去 404。

              而**后端是完整的**（intercept_pending 表 + GET/POST 三个路由 + 写入路径都在），
              只是没有 UI 去看它。所以保留数字、把链接改成不可点，并说明现状。

              见 L27。⚠️ 这个缺口一旦有数据就是 P0：ask 规则会走
              intercept.HandleAsk → 阻塞等待决策 → 而没人能决策 → Agent 卡死。
              （代码侧已加固：ask 强制带 60s 超时，见 timeoutEnabledFor。） */}
          {pendingCount > 0 && (
            <div className="mt-3">
              <div className="mb-1.5 text-[10px] font-medium text-amber-400">待审批 ({pendingCount})</div>
              <div className="flex flex-col gap-1.5">
                {pending.slice(0, 3).map((p) => (
                  // 不可点：用 <div> 而非 <Link>，并去掉 hover 态与箭头图标
                  // （箭头是「可点」的视觉承诺，保留它就是骗人）。
                  <div
                    key={p.id}
                    className="flex items-center justify-between rounded-lg border border-amber-500/20 bg-amber-500/5 px-2.5 py-2"
                  >
                    <div className="min-w-0">
                      <div className="text-[10px] font-medium">{p.tool_name}</div>
                      <div className="text-[9px] text-muted-foreground">{p.agent_name}</div>
                    </div>
                  </div>
                ))}
                {pendingCount > 3 && (
                  <div className="text-center text-[10px] text-muted-foreground">
                    还有 {pendingCount - 3} 条…
                  </div>
                )}
              </div>
              {/* 说明现状而不是承诺未来 ——「将在后续版本提供」那种写法会一直显示
                  到有人做那个页面，而没人知道什么时候做，它会从「说明」变成「长期谎言」。 */}
              <p className="mt-1.5 leading-snug text-[9px] text-muted-foreground">
                {pendingCount} 条工具调用需人工确认。⚠️ 审批界面暂不可用 —— 后端已记录，但当前版本没有查看入口。
                <br />
                超时（默认 60 秒）后系统会按规则自动拒绝。
              </p>
            </div>
          )}

          {pendingCount === 0 && (
            <div className="mt-3 rounded-lg border bg-muted/10 px-3 py-3 text-center text-[10px] text-muted-foreground">
              <ShieldCheckIcon className="mx-auto mb-1 size-4 text-emerald-500/50" />
              无待审批拦截
            </div>
          )}
        </Card>
      </div>
    </div>
  );
}
