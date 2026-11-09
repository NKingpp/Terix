"use client";

// 执行模式的全局状态 + frozen 横幅（WE-13）。
//
// # 为什么挂在 (main)/layout.tsx 而不是 exec 页
//
// 「冻结」是全系统状态，不该只在 /exec 页面可见。
// 用户可能正在 dashboard 看行情，此时账户被冻结 —— 他需要立刻知道。
//
// 而挂在 layout 只改一处，不用每个页面都接 context。
//
// # 三种状态必须区分（这是最容易写错的地方）
//
//   undefined（还没拉到）    → 不显示任何横幅
//   "frozen"                → 显示冻结横幅
//   其它                    → 不显示
//
// 如果横幅条件写成 `mode !== "paper"`，那「加载中」就会显示「已冻结」——
// 一个是等，一个是报，视觉上都是「没有正常状态」，但含义完全不同。

import * as React from "react";

import { LockIcon, TriangleAlertIcon } from "lucide-react";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { api } from "@/lib/api";

// 轮询间隔：60s，不是 10s。
//
// # 为什么 60s 够
//
// 模式是手动切换的（一天可能几次）；冻结由对账自动触发，
// 而对账本身 60s 才跑一次。所以最坏延迟 = 对账 60s + 轮询 60s = 120s。
//
// 10s 轮询会让请求量变 6 倍，但**无法让冻结更早可见**——
// 因为对账本身 60s 才跑一次。这是「轮询快于数据源」的典型浪费。
//
// ⚠️ 如果有人要「优化」成 10s，请先看上面这段。
const POLL_MS = 60_000;

// 连续失败多少次后显示「无法获取」提示。
//
// # 为什么不一次失败就显示
//
// 单次失败（网络抖动）不该打扰用户 —— 保留上次已知状态即可。
// 但连续 3 次失败说明真的连不上，那时「什么都不显示」等于告诉用户「一切正常」。
const FAIL_THRESHOLD = 3;

export interface ExecModeState {
  /** undefined 表示「还没拉到」—— 与「已冻结」是两件事。 */
  mode: string | undefined;
  frozen: boolean;
  fingerprint: string;
  limitsRevision: number;
  /** loaded 为 true 表示至少成功拉到过一次。 */
  loaded: boolean;
  /** unreachable 为 true 表示连续 N 次拉取失败。 */
  unreachable: boolean;
  reload: () => void;
}

const Ctx = React.createContext<ExecModeState | null>(null);

export function useExecMode(): ExecModeState {
  const v = React.useContext(Ctx);
  if (!v) {
    throw new Error("useExecMode 必须在 ExecModeProvider 内使用");
  }
  return v;
}

export function ExecModeProvider({ children }: { children: React.ReactNode }) {
  const [mode, setMode] = React.useState<string | undefined>(undefined);
  const [fingerprint, setFingerprint] = React.useState("");
  const [limitsRevision, setLimitsRevision] = React.useState(0);
  const [loaded, setLoaded] = React.useState(false);
  const [failStreak, setFailStreak] = React.useState(0);
  const [tick, setTick] = React.useState(0);

  const reload = React.useCallback(() => setTick((n) => n + 1), []);

  // tick 的唯一作用是「手动触发重跑」—— 它不出现在 effect 体内，但必须作为
  // 依赖存在。这不是「多余依赖」，是「重跑按钮」的实现方式：改 tick 让 effect 重新执行。
  // biome-ignore lint/correctness/useExhaustiveDependencies: 见上
  React.useEffect(() => {
    let cancelled = false;

    async function pull() {
      try {
        const r = await api.execMode();
        if (cancelled) return;
        setMode(r.mode);
        setFingerprint(r.rules_fingerprint);
        setLimitsRevision(r.limits_revision);
        setLoaded(true);
        setFailStreak(0);
      } catch {
        if (cancelled) return;
        // 不清空 mode —— 保留上次已知值。
        // 「拿不到最新」和「没有状态」不同：前者可以继续用旧值，
        // 后者只能显示「未知」。
        setFailStreak((n) => n + 1);
      }
    }

    void pull();
    const id = window.setInterval(() => void pull(), POLL_MS);
    return () => {
      cancelled = true;
      window.clearInterval(id);
    };
  }, [tick]);

  const state: ExecModeState = {
    mode,
    frozen: mode === "frozen",
    fingerprint,
    limitsRevision,
    loaded,
    unreachable: failStreak >= FAIL_THRESHOLD,
    reload,
  };

  return (
    <Ctx.Provider value={state}>
      <ExecBanner state={state} />
      {children}
    </Ctx.Provider>
  );
}

// ExecBanner 是常驻横幅。三种状态三种呈现。
function ExecBanner({ state }: { state: ExecModeState }) {
  // ① 连续失败：黄色提示「无法获取」。
  //
  // 为什么不是红色：「后端连不上」不等于「已冻结」——两者的应对不同。
  // 红色横幅会让用户以为「我的实盘被冻结了」，而实际只是网络问题。
  if (state.unreachable) {
    return (
      <div className="px-4 pt-3">
        <Alert>
          <TriangleAlertIcon className="size-4" />
          <AlertTitle>无法获取执行模式</AlertTitle>
          <AlertDescription>
            已连续多次拉取失败。当前显示的状态可能已过期 —— 请检查后端是否在运行。这不代表系统已冻结。
          </AlertDescription>
        </Alert>
      </div>
    );
  }

  // ② 还没拉到过：不显示（不要用「加载中」占位，那会闪一下）。
  if (!state.loaded) {
    return null;
  }

  // ③ 已冻结：红色常驻。
  if (state.frozen) {
    return (
      <div className="px-4 pt-3">
        <Alert variant="destructive">
          <LockIcon className="size-4" />
          <AlertTitle>执行已冻结</AlertTitle>
          <AlertDescription>
            系统已停止一切自动执行（含模拟盘）。请到
            <a className="mx-1 underline" href="/exec">
              执行模式页
            </a>
            查看原因并解冻。
          </AlertDescription>
        </Alert>
      </div>
    );
  }

  // ④ 正常：不显示。
  return null;
}
