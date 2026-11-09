"use client";

// 行情渠道凭证管理页。
//
// 本页只负责编排：加载 meta + 列表、维护表单状态、调用接口、编排 Sheet。
// 字段规格在 _components/provider-fields.ts，卡片在 _components/credential-card.tsx，
// 表单控件在 _components/credential-form.tsx —— 拆开的理由与 notify 一致：
// 卡片/表单各自能被单独读懂，挤在一个文件里页面会失控。
//
// # 本页最硬的一条约束
//
// **「哪些字段算凭据」完全来自后端 meta，这里一个字都不硬编码。**
// 凭证硬编码的后果是密钥明文上屏（截屏/录屏都会带走），比 notify 的同名问题
// 严重两个数量级。见 _components/provider-fields.ts 顶部注释与设计文档 §10。

import * as React from "react";

import { Loader2Icon, PlusIcon } from "lucide-react";
import { toast } from "sonner";

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Switch } from "@/components/ui/switch";
import { api, errCode, errStatus } from "@/lib/api";
import type { MarketCredential, MarketProviderMeta } from "@/lib/types";

import { CredentialCard } from "./_components/credential-card";
import { CredentialForm, type CredForm, emptyForm } from "./_components/credential-form";
import {
  dynamicFieldSpecs,
  normalizeValue,
  providerLabel,
  purposeCopyFor,
  supportsTextSecurity,
  type FieldSpec,
} from "./_components/provider-fields";
import { metaFor, PURPOSE_DESCRIPTIONS, PURPOSE_LABELS, purposeKey, type CredentialPurpose } from "@/lib/credential-purpose";

/**
 * 按 meta 的 fields[].default 生成初始值表。
 *
 * # 为什么 checkbox 也在这里给初值
 *
 * OKX 的「模拟盘」开关如果初始为空串，勾选框渲染成未勾选（value !== "true"），
 * 而用户拿模拟盘 key 时**必须**勾上 —— 让用户先发现「这里有个勾选框，
 * 而且我大概该勾」比让他撞一次 50101 再回来找要省事得多。
 *
 * 但「默认勾选」不能写死成 true：实盘用户会误勾，然后得到
 * 「key 与环境不匹配」。所以默认值也来自后端 fields[].default，
 * 由 meta 决定某渠道某个 checkbox 该不该预勾。
 *
 * ⚠️ 当前 OKX 的 demo 字段没有给 default（默认不勾）。
 * 理由：实盘与模拟盘的误判代价不对称 —— 实盘 key 被误标成模拟盘会直接
 * 50101 失败（安全），而模拟盘 key 忘了勾只是验证不过（可重试）。
 * 所以保守地不预勾，靠错误文案引导。
 */
function defaultValueMap(
  providers: MarketProviderMeta[],
  provider: string,
  purpose: CredentialPurpose,
): Record<string, string> {
  const meta = metaFor(providers, provider, purpose);
  if (!meta) return {};
  const out: Record<string, string> = {};
  for (const f of meta.fields ?? []) {
    if (f.default) out[f.key] = f.default;
  }
  return out;
}

/** 把一个 provider 的动态字段值组装成提交体。 */
function buildPayload(
  form: CredForm,
  fields: FieldSpec[],
  existing: MarketCredential | null,
): Record<string, unknown> {
  const out: Record<string, unknown> = {
    provider: form.provider,
    // 新建时必须传 kind；编辑时 db 层按「空串=没提这件事」处理，所以传了也无害。
    kind: form.purpose,
    account_label: form.account_label.trim(),
    enabled: form.enabled,
  };
  // 三个「意图」用三个字段区分（后端 §Q4/Q10）：
  //   编辑 → 传 id（允许改 label）
  //   新增 → create_only: true（撞名 409）
  // 注意：编辑时**不要**传 create_only，否则改 label 会被当成新增而 409。
  if (existing) {
    out.id = existing.id;
  } else {
    out.create_only = true;
  }

  // # extra 的字段级清空（为什么要在前端合并）
  //
  // 后端 extra 的语义是「传了就整体替换」（SQL 里是 COALESCE($::jsonb, extra)），
  // **不支持单个 key 的清空**。如果用户在有两个 extra 字段时只想清掉其中一个，
  // 传 {a: ""} 会把 b 也一起丢掉 —— 因为整体替换。
  //
  // 解法在前端：extra 是**非敏感**字段，GET 会完整返回（editing.extra），
  // 所以前端持有旧值，可以「读旧 → 改目标 → 整体写回」。
  // 这样后端不用改，而语义变成字段级。
  //
  // 如果将来 extra 里混入敏感值（不该发生，见后端注释），这条路会失效 ——
  // 那时 GET 不再返回全量，就必须改后端。但那时应该整体换 secrets/secret_hints 方案。
  const extra: Record<string, string> = { ...(existing?.extra ?? {}) };
  let extraTouched = false;

  for (const f of fields) {
    if (form.clearing[f.key]) {
      if (f.storage === "extra") {
        // extra 字段的清空 = 从合并后的对象里删掉该 key（而不是写空串，
        // 因为空串本身在某些 provider 里可能是合法值）。
        delete extra[f.key];
        extraTouched = true;
        continue;
      }
      // 敏感字段：显式清空是 API 魔法值。这是唯一一处把 UI 状态翻译成 <clear>。
      out[f.key] = "<clear>";
      continue;
    }
    const raw = form.values[f.key] ?? "";
    const v = normalizeValue(f, raw);
    if (v === "") {
      // 空串 = 保留旧值（后端已定的语义）。新增时没有旧值可保留，
      // 效果等同「未提供」，后端按 required 校验报错 —— 与前端拦的结果一致。
      continue;
    }
    if (f.storage === "extra") {
      extra[f.key] = v;
      extraTouched = true;
    } else {
      out[f.key] = v;
    }
  }
  // 只在真的动过 extra 时才传 —— 否则会把「未触碰」误当成「清空其余」。
  // 注意：这里传的是**合并后的完整对象**（含未修改的旧键），不是增量。
  if (extraTouched) {
    out.extra = extra;
  }
  return out;
}

export default function CredentialsPage() {
  const [providers, setProviders] = React.useState<MarketProviderMeta[]>([]);
  const [creds, setCreds] = React.useState<MarketCredential[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [loadError, setLoadError] = React.useState<string | null>(null);

  const [sheetOpen, setSheetOpen] = React.useState(false);
  const [editing, setEditing] = React.useState<MarketCredential | null>(null);
  const [form, setForm] = React.useState<CredForm>(emptyForm());
  const [saving, setSaving] = React.useState(false);
  // 当前 Tab：交易通道 / 数据源。
  //
  // # 为什么用 Tab 而不是在一个列表里混排
  //
  // 同一个 provider 会出现在两个 Tab 里（okx 的交易凭据与行情凭据），
  // 而它们的字段布局与用途完全不同。混排会让用户分不清「我这一条是干什么的」。
  const [purposeTab, setPurposeTab] = React.useState<CredentialPurpose>("trade");
  const [fieldError, setFieldError] = React.useState<string | null>(null);

  const [deleteTarget, setDeleteTarget] = React.useState<MarketCredential | null>(null);
  const [deleting, setDeleting] = React.useState(false);

  // 页面级：同一时刻只允许一个验证在跑。
  // 不这样限制的话，连点会拉起多个 CLI 子进程（冷启动几百毫秒），
  // 后端也可能对同一凭证加锁 → 前端拿到一堆 429/冲突。
  const [verifyingId, setVerifyingId] = React.useState<number | null>(null);
  // 已确认不支持验证的渠道（点过拿到 501）→ 按钮永久隐藏。只存组件 state，
  // 不写进浏览器持久化存储（那会让「不支持」这个信息跨会话残留，
  // 而它可能随后端升级改变）。
  const [unsupportedProviders, setUnsupportedProviders] = React.useState<Set<string>>(new Set());

  const [textSecuritySupported] = React.useState(() => supportsTextSecurity());

  const load = React.useCallback(async () => {
    try {
      const r = await api.marketCredentials();
      setProviders(r.meta?.providers ?? []);
      setCreds(r.credentials ?? []);
      setLoadError(null);
    } catch (e) {
      setLoadError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  // 页面卸载时清空表单 state —— 避免下次进入时旧值还在（含已输入的密钥）。
  React.useEffect(() => {
    return () => setForm(emptyForm());
  }, []);

  // ⚠️ 必须带 purpose —— marketProviders 里 provider 会重复（okx 有 trade/data 两项），
  // 只按 provider 查会取到 trade 那项，于是编辑 data 凭据时显示的是 trade 的字段布局。
  const metaOf = (provider: string, purpose: CredentialPurpose) =>
    metaFor(providers, provider, purpose);

  const currentMeta = metaOf(form.provider, form.purpose);
  const currentCopy = purposeCopyFor(form.provider, form.purpose);
  const currentFields = currentMeta ? dynamicFieldSpecs(currentMeta, currentCopy) : [];

  /** 打开「新增」Sheet。provider 默认选第一个有字段的。 */
  const openAdd = () => {
    const usable = providers.find((p) => (p.fields?.length ?? 0) > 0 || p.secret_fields.length > 0);
    setEditing(null);
    const base = emptyForm(usable?.provider ?? "", usable?.purpose ?? "trade");
    setForm({
      ...base,
      values: defaultValueMap(providers, base.provider, base.purpose),
    });
    setFieldError(null);
    setSheetOpen(true);
  };

  /** 打开「编辑」Sheet。secret 字段一律留空（不回显旧值）。 */
  const openEdit = (c: MarketCredential) => {
    setEditing(c);
    setForm({
      provider: c.provider,
      purpose: c.kind,
      account_label: c.account_label,
      base_url: c.base_url,
      enabled: c.enabled,
      values: { ...c.extra }, // extra 是非敏感字段，可回显
      clearing: {},
    });
    setFieldError(null);
    setSheetOpen(true);
  };

  /** 切换 provider/purpose 时重置动态字段。 */
  const onProviderChange = (next: string, nextPurpose: CredentialPurpose) => {
    // 只保留通用字段，provider 专属字段全部清空 ——
    // 否则会把 okx 的 api_key 填进 fosun 的同名 input 里，用户看不出来。
    // 切 provider 时保留 purpose（用户选的是「OKX 的行情凭据」这种组合，
    // 换 provider 不该把用途一起重置）。
    setForm((f) => ({
      ...emptyForm(next, nextPurpose),
      account_label: f.account_label,
      enabled: f.enabled,
      // 切渠道后按新渠道的 fields[].default 预填（endpoint 这类长 URL）。
      values: defaultValueMap(providers, next, nextPurpose),
    }));
    setFieldError(null);
  };

  /**
   * 保存前的前置检查（明文降级 + 必填）。
   *
   * 返回 true 表示可以继续。
   */
  const confirmBeforeSave = async (): Promise<boolean> => {
    // 1. 必填校验（required 来自后端）
    for (const f of currentFields) {
      if (!f.required) continue;
      if (form.clearing[f.key]) continue; // 已标记清除不算「没填」
      if (editing && f.kind === "password") continue; // 编辑时留空 = 保留旧值
      if ((form.values[f.key] ?? "").trim() === "") {
        setFieldError(`请填写「${f.label}」`);
        return false;
      }
    }
    // 2. 明文降级二次确认。
    //    只在「password + 多行 + 浏览器不支持遮罩 + 字段有值」时触发。
    //    警告条会被习惯性忽略，必须在动作发生的那一刻打断。
    const degraded = currentFields.some(
      (f) =>
        f.kind === "password" &&
        f.multiline &&
        !textSecuritySupported &&
        !form.clearing[f.key] &&
        (form.values[f.key] ?? "").trim() !== "",
    );
    if (!degraded) return true;
    // # 为什么用 window.confirm 而不是 AlertDialog
    //
    // confirmBeforeSave 是 async 函数里的**同步判断点**，而 AlertDialog 需要
    // Promise 包装（setPending + resolve 回调）+ 处理 Esc / 点外部关闭的
    // resolve(false)，约 20 行状态机。而这个确认只在
    // 「Firefox（不支持 -webkit-text-security）+ 填了多行私钥」时触发，是罕见场景。
    //
    // 原生弹窗的视觉不一致是可接受的代价 —— 它**无法被忽略**，而这正是二次确认
    // 的目的（警告条会被习惯性跳过，弹窗必须点掉才能继续）。
    //
    // ⚠️ 如果将来要统一视觉改成 AlertDialog，最容易漏的一处是：
    //    onOpenChange 里必须 resolve(false)。
    //    漏了会导致用户点空白处取消时 Promise 永不 resolve —— 保存按钮永久 loading，
    //    只能刷新页面。这是这类「命令式弹窗」最典型的 bug。
    return window.confirm(
      "私钥将以明文显示\n\n" +
        "当前浏览器无法遮罩多行私钥，提交时私钥会明文显示在屏幕上。\n" +
        "如果你正在录屏、共享屏幕或有人在旁，请先停止。\n\n" +
        "建议改用 Chrome / Edge / Safari，或将 PEM 压成单行后使用单行密码框。",
    );
  };

  const save = async () => {
    if (!form.provider) {
      setFieldError("请选择渠道");
      return;
    }
    setFieldError(null);
    if (!(await confirmBeforeSave())) return;

    setSaving(true);
    try {
      await api.marketSaveCredential(
        buildPayload(form, currentFields, editing) as Parameters<typeof api.marketSaveCredential>[0],
      );
      // 顺序是有意的：先清敏感值，再关 Sheet，最后刷新。
      // 反过来（先关再清）会有一帧状态：Sheet 已卸载但 state 还在闭包里。
      setForm(emptyForm());
      setSheetOpen(false);
      await load();
      toast.success("已保存");
    } catch (e) {
      // **不清空表单、不关闭 Sheet**。
      // 用户刚输入的密钥只存在于内存里，清掉就得回去重新生成 API key ——
      // 那是不可恢复的损失（第三方通常只在创建时显示一次）。
      const code = errCode(e);
      const msg = (e as Error).message;
      if (code === "CREDENTIAL_EXISTS") {
        setFieldError("该渠道下已存在同名账户，请换一个账户标签，或直接编辑已有的那条。");
      } else if (code === "LABEL_CONFLICT") {
        setFieldError("该账户标签已被另一条凭证占用，请换一个。");
      } else if (code === "PROVIDER_IMMUTABLE") {
        setFieldError("渠道不可修改。如需更换渠道，请删除这条后重新添加。");
      } else {
        toast.error(`保存失败：${msg}`);
      }
    } finally {
      setSaving(false);
    }
  };

  /** 只改 enabled：走 upsert（省略 id 与 create_only → 按 provider+label 覆盖）。 */
  const toggleEnabled = async (c: MarketCredential, next: boolean) => {
    // 乐观更新 + 失败回滚
    setCreds((list) => list.map((x) => (x.id === c.id ? { ...x, enabled: next } : x)));
    try {
      await api.marketSaveCredential({
        provider: c.provider,
        account_label: c.account_label,
        enabled: next,
      });
    } catch (e) {
      setCreds((list) => list.map((x) => (x.id === c.id ? { ...x, enabled: c.enabled } : x)));
      toast.error(`切换失败：${(e as Error).message}`);
    }
  };

  const doDelete = async () => {
    if (!deleteTarget) return;
    setDeleting(true);
    try {
      await api.marketDeleteCredential(deleteTarget.id);
      setDeleteTarget(null);
      await load();
      toast.success("已删除");
    } catch (e) {
      toast.error(`删除失败：${(e as Error).message}`);
    } finally {
      setDeleting(false);
    }
  };

  /**
   * 验证。
   *
   * 三种结果必须分开处理（「没验成」不能显示成「验证失败」）：
   *   200 + ok=true   → toast 成功，徽章转绿（用响应里的凭证立即更新，不等刷新）
   *   200 + ok=false  → **业务失败**：不弹 toast，改用卡片内折叠区展示原因
   *   throw 5xx/网络  → **没验成**：只弹 toast，**不改徽章**
   *   501             → 该渠道不支持，隐藏按钮
   */
  const doVerify = async (c: MarketCredential) => {
    setVerifyingId(c.id);
    try {
      const r = await api.marketVerifyCredential(c.id);
      // 无论 ok 与否，后端都返回了最新的凭证记录（含 verified_at / verify_error）。
      setCreds((list) => list.map((x) => (x.id === c.id ? r.credential : x)));
      if (r.ok) {
        toast.success("验证通过");
      }
      // ok=false 不弹 toast：verify_error 可能是很长的 CLI stderr，
      // 塞进 toast 会溢出且不可读。卡片内展开才是对的。
    } catch (e) {
      const status = errStatus(e);
      const code = errCode(e);
      if (status === 501 || code === "MARKET_CREDENTIAL_VERIFY_UNSUPPORTED") {
        setUnsupportedProviders((s) => new Set(s).add(c.provider));
        toast.info("该渠道暂不支持在线验证，凭证会在首次使用时检验");
        return;
      }
      // 网络/5xx：**不改徽章**。不能把「验证没做成」显示成「验证失败」——
      // 那会让用户去重打一个可能正确的 key。
      toast.error(`验证请求失败：${(e as Error).message}`);
    } finally {
      setVerifyingId(null);
    }
  };

  // 空状态（无任何凭证）
  const isEmpty = !loading && creds.length === 0 && !loadError;

  return (
    <div className="flex flex-col gap-4 p-4 sm:p-6">
      <Card>
        <CardHeader>
          <CardTitle className="text-base">行情渠道凭证</CardTitle>
          <CardDescription>
            本页管理 okx 与星财富的凭证。同花顺 / 通达信的连接由系统内置（走 Connector
            与本地客户端），无需在此配置；它们的连通状态见仪表盘的「数据源健康」。
          </CardDescription>
        </CardHeader>
        <CardContent>
          <p className="text-muted-foreground text-xs">
            凭证保存在<strong className="text-foreground">本机数据库</strong>
            ，仅用于访问对应的第三方服务。已保存的密钥不会回显到页面，
            留空即保持原值。
          </p>
        </CardContent>
      </Card>

      {loadError && (
        <Card>
          <CardContent className="pt-6">
            <p className="text-destructive text-sm">加载失败：{loadError}</p>
            <Button size="sm" variant="outline" className="mt-2" onClick={() => void load()}>
              重试
            </Button>
          </CardContent>
        </Card>
      )}

      {loading ? (
        <div className="text-muted-foreground flex items-center gap-2 p-6 text-sm">
          <Loader2Icon className="size-4 animate-spin" />
          加载中…
        </div>
      ) : isEmpty ? (
        <Empty>
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <PlusIcon />
            </EmptyMedia>
            <EmptyTitle>还没有配置任何行情渠道凭证</EmptyTitle>
            <EmptyDescription>
              添加 okx 或星财富的凭证后，相关分析与交易才能取到数据。凭证保存在本机数据库。
            </EmptyDescription>
          </EmptyHeader>
          <EmptyContent>
            <Button onClick={openAdd} disabled={providers.length === 0}>
              <PlusIcon className="size-4" /> 添加凭证
            </Button>
            {providers.length === 0 && (
              <p className="text-muted-foreground text-xs">后端尚未提供任何渠道的字段定义。</p>
            )}
          </EmptyContent>
        </Empty>
      ) : (
        <>
          {/* Tab：交易通道 / 数据源。
              两个 Tab 各自只列本用途的凭据 —— 因为凭据的字段布局与用途完全不同。 */}
          <div className="mb-4 flex gap-2 border-b">
            {(["trade", "data"] as const).map((p) => {
              const count = creds.filter((c) => c.kind === p).length;
              const active = purposeTab === p;
              return (
                <button
                  key={p}
                  type="button"
                  onClick={() => setPurposeTab(p)}
                  className={
                    "-mb-px border-b-2 px-3 py-2 text-sm transition " +
                    (active
                      ? "border-foreground text-foreground font-medium"
                      : "text-muted-foreground hover:text-foreground border-transparent")
                  }
                >
                  {PURPOSE_LABELS[p]}
                  {count > 0 && <span className="text-muted-foreground ml-1.5 text-xs">{count}</span>}
                </button>
              );
            })}
          </div>
          <p className="text-muted-foreground mb-4 text-xs">{PURPOSE_DESCRIPTIONS[purposeTab]}</p>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
          <button
            type="button"
            onClick={openAdd}
            className="text-foreground/70 border-foreground/70 hover:bg-muted/60 flex min-h-[130px] flex-col items-center justify-center gap-2 rounded-xl border border-dashed transition hover:shadow-sm"
          >
            <PlusIcon className="size-6" />
            <span className="text-sm">添加凭证</span>
          </button>

          {creds
            .filter((c) => c.kind === purposeTab)
            .map((c) => {
            const pm = metaOf(c.provider, c.kind);
            return (
              // 卡片的点击由 CredentialCard 自己承接（它是 role=button 的整体可点元素）。
              // 不在外层再包一个 role="button" 的 div —— 那会造出嵌套的交互元素，
              // 读屏会报两个按钮，而内层卡片本来就有正确的语义。
              <CredentialCard
                key={c.id}
                cred={c}
                verifiable={pm?.verifiable ?? false}
                unsupported={unsupportedProviders.has(c.provider)}
                verifyingId={verifyingId}
                onEdit={() => openEdit(c)}
                onToggleEnabled={(next) => void toggleEnabled(c, next)}
                onDelete={() => setDeleteTarget(c)}
                onVerify={() => void doVerify(c)}
              />
            );
            })}
          </div>
        </>
      )}

      <Sheet
        open={sheetOpen}
        onOpenChange={(v) => {
          setSheetOpen(v);
          // 关闭时清空表单（不是留着下次复用）—— 已输入的密钥不该在 state 里跨打开存活。
          if (!v) setForm(emptyForm());
        }}
      >
        <SheetContent side="right" className="w-full data-[side=right]:sm:max-w-lg">
          <SheetHeader>
            <SheetTitle>{editing ? "编辑凭证" : "添加凭证"}</SheetTitle>
            <SheetDescription>
              {form.provider ? providerLabel(form.provider, form.purpose, currentMeta?.label) : "选择渠道后填写字段"}
            </SheetDescription>
          </SheetHeader>

          <div className="flex min-h-0 flex-1 flex-col overflow-y-auto px-4">
            <form
              autoComplete="off"
              className="grid gap-4 py-4"
              onSubmit={(e) => {
                e.preventDefault();
                void save();
              }}
            >
              <fieldset disabled={saving} className="grid gap-4 border-0 p-0">
                <div className="grid gap-2">
                  <Label>渠道</Label>
                  {/* value 是 provider:purpose 复合键 —— 与下方 SelectItem 的 value 一致。
                      用裸 provider 会撞（okx 有两项）。 */}
                  <Select
                    value={purposeKey(form.provider, form.purpose)}
                    onValueChange={(v) => {
                      // 复合键拆回 provider + purpose，两者一起更新。
                      const i = v.indexOf(":");
                      onProviderChange(v.slice(0, i), v.slice(i + 1) as CredentialPurpose);
                    }}
                    disabled={!!editing}
                  >
                    <SelectTrigger>
                      <SelectValue placeholder="选择渠道" />
                    </SelectTrigger>
                    <SelectContent>
                      {/* provider 列表来自后端 meta —— 后端没给的渠道不出现。
                          pending（无字段）的渠道 disabled 而不是隐藏：
                          用户需要知道「系统打算支持，只是还没做」。 */}
                      {/* provider 现在会重复（okx 有 trade/data 两项），
                          所以 key 和 value 都必须用 provider:purpose 复合键。 */}
                      {providers.map((p) => {
                        const pending =
                          (p.fields?.length ?? 0) === 0 && p.secret_fields.length === 0;
                        return (
                          <SelectItem
                            key={purposeKey(p.provider, p.purpose)}
                            value={purposeKey(p.provider, p.purpose)}
                            disabled={pending}
                          >
                            {providerLabel(p.provider, p.purpose, p.label)}
                            {pending && <span className="text-muted-foreground ml-2">（字段待定）</span>}
                          </SelectItem>
                        );
                      })}
                    </SelectContent>
                  </Select>
                  {editing && (
                    <p className="text-muted-foreground text-xs">
                      渠道不可修改——改了渠道等于换一套凭证，请新建一条。
                    </p>
                  )}
                </div>

                {currentCopy?.description && (
                  <p className="text-muted-foreground text-xs">{currentCopy.description}</p>
                )}
                {currentCopy?.docHint && (
                  <p className="text-muted-foreground text-xs">💡 {currentCopy.docHint}</p>
                )}

                <div className="grid gap-2">
                  <Label htmlFor="cred-label">账户标签</Label>
                  <Input
                    id="cred-label"
                    type="text"
                    autoComplete="off"
                    placeholder="例如：主账户 / 模拟盘"
                    value={form.account_label}
                    onChange={(e) => setForm({ ...form, account_label: e.target.value })}
                  />
                  <p className="text-muted-foreground text-xs">
                    仅用于区分同一渠道下的多个账户；留空显示为「未命名账户」。
                  </p>
                </div>

                {currentMeta?.supports_base_url && (
                  <div className="grid gap-2">
                    <Label htmlFor="cred-base">自定义地址</Label>
                    <Input
                      id="cred-base"
                      type="text"
                      autoComplete="off"
                      placeholder="留空使用官方地址"
                      value={form.base_url}
                      onChange={(e) => setForm({ ...form, base_url: e.target.value })}
                    />
                  </div>
                )}

                {currentFields.length === 0 ? (
                  <div className="border-muted-foreground/40 rounded-md border border-dashed p-4">
                    <p className="text-sm font-medium">该渠道的凭证字段尚未就绪</p>
                    {/*
                      必须显式说明「能改什么、不能改什么」。
                      不说明的话，用户会疑惑「为什么我只能改名字不能改密码」——
                      看起来像 bug。而且保存按钮是**可用的**（见下方 disabled 条件），
                      不说明会让人以为点错了。
                    */}
                    <p className="text-muted-foreground mt-1 text-xs">
                      {providerLabel(form.provider, form.purpose, currentMeta?.label)}
                      的凭证字段配置尚未就绪，你可以修改<strong className="text-foreground">账户名称</strong>和
                      <strong className="text-foreground">启用状态</strong>，但无法修改凭证内容。
                      等后端配置就绪后再回来填写。
                    </p>
                  </div>
                ) : (
                  <CredentialForm
                    fields={currentFields}
                    form={form}
                    onChange={setForm}
                    existing={editing ? { api_key_hint: editing.api_key_hint } : null}
                    disabled={saving}
                    textSecuritySupported={textSecuritySupported}
                    fieldError={fieldError}
                  />
                )}

                <div className="flex items-center gap-2">
                  <Switch
                    id="cred-enabled"
                    checked={form.enabled}
                    onCheckedChange={(v) => setForm({ ...form, enabled: v })}
                  />
                  <Label htmlFor="cred-enabled">启用</Label>
                </div>
              </fieldset>

              <div className="flex items-center gap-2 border-t pt-4">
                <Button
                  type="submit"
                  /*
                    字段为空时**仍允许保存** —— 此时只提交 provider / account_label /
                    enabled，不碰凭证内容。

                    为什么要允许：字段配置可能因后端调整而暂时为空，但用户改个账户名
                    或停用这条凭证的需求不依赖那些字段。禁掉保存会让用户完全卡住
                    （连停用都做不到 —— 而停用恰恰是「暂时不用但有隐患」时的正确操作）。

                    必填校验在 confirmBeforeSave 里做（那里能看到具体字段），
                    这里只按「有没有字段」区分。
                  */
                  disabled={saving}
                  onClick={() => void save()}
                >
                  {saving ? (
                    <>
                      <Loader2Icon className="size-4 animate-spin" /> 保存中…
                    </>
                  ) : (
                    "保存"
                  )}
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  disabled={saving}
                  onClick={() => {
                    setSheetOpen(false);
                    setForm(emptyForm());
                  }}
                >
                  取消
                </Button>
              </div>
            </form>
          </div>
        </SheetContent>
      </Sheet>

      <AlertDialog open={deleteTarget !== null} onOpenChange={(v) => !v && setDeleteTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              删除「{deleteTarget?.account_label || "未命名账户"}」的{" "}
              {providerLabel(deleteTarget?.provider ?? "", deleteTarget?.kind)} 凭证？
            </AlertDialogTitle>
            <AlertDialogDescription asChild>
              <div className="space-y-2 text-sm">
                <p>删除后该渠道将立即失效，相关分析与交易无法继续使用它。</p>
                <p className="text-destructive font-medium">
                  凭证内容不可恢复 —— 重新添加时你需要再次输入完整的 API Key / Secret /
                  Passphrase。如果你只是暂时不用，请用卡片右上角的开关停用，而不是删除。
                </p>
              </div>
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleting}>取消</AlertDialogCancel>
            <AlertDialogAction
              disabled={deleting}
              onClick={(e) => {
                e.preventDefault();
                void doDelete();
              }}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              {deleting ? "删除中…" : "删除凭证"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
