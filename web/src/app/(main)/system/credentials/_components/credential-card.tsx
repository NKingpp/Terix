"use client";

// 凭证卡片：四态徽章 + 验证按钮 + 失败原因折叠区。
//
// 拆成独立组件而不是内联在页面里，理由与 notify 的 DeliveryList 一致：
// 卡片的交互密度（开关/删除/验证/折叠展开）比列表容器高，混在一起会失控。

import * as React from "react";

import {
  AlertTriangleIcon,
  CheckIcon,
  ChevronDownIcon,
  CopyIcon,
  EraserIcon,
  Loader2Icon,
  ShieldCheckIcon,
  Trash2Icon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Switch } from "@/components/ui/switch";
import type { MarketCredential } from "@/lib/types";

import { providerLabel, VERIFY_STATE_UI, verifyStateOf } from "./provider-fields";

interface Props {
  cred: MarketCredential;
  /** 后端 provider 元数据里的可验证标志（false = 不渲染验证按钮）。 */
  verifiable: boolean;
  /** 该渠道已确认不支持验证（点过拿到 501）→ 按钮永久隐藏。 */
  unsupported: boolean;
  /** 正在验证的凭证 id（页面级，保证同一时刻只跑一个）。 */
  verifyingId: number | null;
  onEdit: () => void;
  onToggleEnabled: (next: boolean) => void;
  onDelete: () => void;
  onVerify: () => void;
}

/** 失败原因折叠区。 */
function VerifyErrorDetail({ text }: { text: string }) {
  const [open, setOpen] = React.useState(false);
  const body = text || "（服务端未提供具体原因）";
  const lineCount = body.split("\n").length;

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(body);
      toast.success("已复制失败原因");
    } catch {
      // 剪贴板可能被浏览器策略拒绝（非 https / 无用户手势）。
      // 不弹错误 —— 用户可以直接手选文本，这里失败不是致命问题。
      toast.error("复制失败，请手动选择文本复制");
    }
  };

  return (
    <Collapsible open={open} onOpenChange={setOpen}>
      <CollapsibleTrigger asChild>
        <Button variant="ghost" size="sm" className="text-destructive h-7 px-2 text-xs">
          <AlertTriangleIcon className="size-3.5" />
          查看失败原因
          <ChevronDownIcon className={`size-3.5 transition-transform ${open ? "rotate-180" : ""}`} />
        </Button>
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className="bg-muted mt-2 rounded-md p-3">
          {/* max-h-48 必限高：CLI stderr 可能几百行。
              whitespace-pre-wrap 保留原始换行（stderr 的缩进有意义），
              break-all 让长 token 强制断行防横向滚动。 */}
          <pre className="max-h-48 overflow-auto font-mono text-xs break-all whitespace-pre-wrap">{body}</pre>
          <div className="mt-2 flex items-center gap-2">
            <Button size="sm" variant="outline" onClick={() => void copy()}>
              <CopyIcon className="size-3.5" /> 复制
            </Button>
            <span className="text-muted-foreground text-xs">共 {lineCount} 行</span>
          </div>
        </div>
      </CollapsibleContent>
    </Collapsible>
  );
}

export function CredentialCard({
  cred,
  verifiable,
  unsupported,
  verifyingId,
  onEdit,
  onToggleEnabled,
  onDelete,
  onVerify,
}: Props) {
  const state = verifyStateOf(cred);
  const ui = VERIFY_STATE_UI[state];
  const verifying = verifyingId === cred.id;

  // 验证按钮的文案随状态变（设计文档 §2.4）：
  //   verified → 重新验证（允许复验，用户可能刚改了远端权限）
  //   stale    → 验证（高亮，这是最该点的场景）
  //   unverified → 验证
  //   failed   → 重试验证
  const verifyLabel =
    state === "verified" ? "重新验证" : state === "failed" ? "重试验证" : "验证";
  const verifyHighlight = state === "stale";

  // 渠道不支持验证（后端 verifiable=false 或已拿到 501）→ 不渲染按钮。
  // 不渲染而不是 disabled：一个永久禁用的按钮会让人一直想「为什么不能点」。
  const showVerify = verifiable && !unsupported;

  const label = cred.account_label || "未命名账户";

  return (
    <Card
      className="hover:border-primary/60 cursor-pointer gap-3 transition hover:shadow-sm"
      onClick={onEdit}
      onKeyDown={(e) => {
        // 卡片整体可点（进入编辑），但要保留键盘可达性：
        // Enter / Space 等同点击，否则键盘用户无法打开编辑。
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onEdit();
        }
      }}
      role="button"
      tabIndex={0}
    >
      <CardHeader>
        <div className="flex items-center gap-2">
          <ShieldCheckIcon className="text-muted-foreground size-4 shrink-0" />
          <CardTitle className="truncate text-base" title={label}>
            {label}
          </CardTitle>
          {/* 卡片整体可点（进入编辑），所以这两个控件必须各自吞掉冒泡，
              否则开关/删除会顺带触发编辑。挂在控件自己身上而不是套一层 div ——
              套 div 会造出「看起来可交互但没有角色」的静态元素。 */}
          <div className="ml-auto flex items-center gap-2">
            <Switch
              checked={cred.enabled}
              onCheckedChange={(v) => onToggleEnabled(v)}
              onClick={(e) => e.stopPropagation()}
              aria-label="启用"
            />
            <Button
              size="icon"
              variant="outline"
              aria-label="删除"
              onClick={(e) => {
                e.stopPropagation();
                onDelete();
              }}
            >
              <Trash2Icon className="text-destructive" />
            </Button>
          </div>
        </div>
      </CardHeader>

      <CardContent className="grid gap-3">
        <div className="flex flex-wrap items-center gap-2">
          <Badge variant="outline">{providerLabel(cred.provider, cred.kind)}</Badge>
          <Badge variant={ui.variant} title={ui.hint}>
            {ui.label}
          </Badge>
          {!cred.enabled && <Badge variant="outline">已停用</Badge>}
          {/* 渠道不支持在线验证时，徽章文案要说清「不是失败，是验不了」——
              显示成「验证失败」会让用户去重打一个可能正确的 key。 */}
          {unsupported && state === "unverified" && (
            <span className="text-muted-foreground text-xs">未验证（渠道不支持在线验证）</span>
          )}
        </div>

        {cred.api_key_hint && (
          <div className="text-muted-foreground font-mono text-xs">
            API Key：<span className="text-foreground">{cred.api_key_hint}</span>
          </div>
        )}

        {cred.base_url && (
          <div className="text-muted-foreground truncate text-xs">
            地址：<span className="text-foreground">{cred.base_url}</span>
          </div>
        )}

        {/* 验证行。即使不显示按钮也保留这一行（放 error 折叠区）——
            否则「验证失败」的卡片会把原因藏起来。 */}
        {(showVerify || state === "failed") && (
          <div className="flex flex-wrap items-center gap-2 border-t pt-2">
            {showVerify && (
              <Button
                size="sm"
                variant={verifyHighlight ? "default" : "outline"}
                disabled={!cred.enabled || verifyingId !== null}
                title={!cred.enabled ? "已停用的凭证不参与验证" : undefined}
                onClick={(e) => {
                  e.stopPropagation();
                  onVerify();
                }}
              >
                {verifying ? (
                  <>
                    <Loader2Icon className="size-3.5 animate-spin" /> 验证中…
                  </>
                ) : state === "verified" ? (
                  <>
                    <CheckIcon className="size-3.5" /> {verifyLabel}
                  </>
                ) : (
                  verifyLabel
                )}
              </Button>
            )}
            {state === "failed" && <VerifyErrorDetail text={cred.verify_error} />}
          </div>
        )}

        {cred.verified_at && (
          <div className="text-muted-foreground text-xs">
            上次验证：{new Date(cred.verified_at).toLocaleString()}
          </div>
        )}

        <div className="text-muted-foreground flex items-center gap-1 text-xs">
          <EraserIcon className="size-3" />
          编辑可修改或清除单个字段；删除会移除整条凭证
        </div>
      </CardContent>
    </Card>
  );
}
