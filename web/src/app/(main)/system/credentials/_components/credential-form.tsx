"use client";

// 凭证表单：动态字段区 + 密码/多行正交渲染 + 清除按钮。
//
// 这个组件只负责「渲染字段与收集输入」，不做提交（提交在 page 里）。
// 拆开的理由：字段渲染的分支比提交逻辑更密（四种控件组合 + 清除态 + 降级告警），
// 混在一起页面会失控。

import { EraserIcon } from "lucide-react";
import type { CredentialPurpose } from "@/lib/types";

import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";

import type { FieldSpec } from "./provider-fields";

/** 表单状态：字段值 + 每个 secret 字段的「待清除」标记。 */
export interface CredForm {
  provider: string;
  /**
   * 凭据用途。与 provider 一起构成「这条凭据是哪种配置项」的身份。
   *
   * # 为什么要放进表单 state
   *
   * 因为字段布局依赖它（trade 要 passphrase，data 不要），
   * 而「当前显示哪套字段」必须随它变。放在外面会让「切 Tab 后表单还是旧的」。
   *
   * ⚠️ 编辑时不可改（与 provider 同理）—— 改用途等于换一条凭据。
   */
  purpose: CredentialPurpose;
  account_label: string;
  base_url: string;
  enabled: boolean;
  /** 动态字段的值，key 是后端字段名。 */
  values: Record<string, string>;
  /** 被标记「清除」的字段（提交时翻译成 `<clear>`）。 */
  clearing: Record<string, boolean>;
}

export function emptyForm(provider = "", purpose: CredentialPurpose = "trade"): CredForm {
  return {
    provider,
    purpose,
    account_label: "",
    base_url: "",
    enabled: true,
    values: {},
    clearing: {},
  };
}

interface Props {
  /** 后端给的字段规格（已由 buildFieldSpecs 组装）。 */
  fields: FieldSpec[];
  form: CredForm;
  onChange: (next: CredForm) => void;
  /** 编辑态才有：用于显示 api_key_hint 与「清除」按钮。 */
  existing?: { api_key_hint: string } | null;
  /** 提交中：禁用整个表单防二次提交。 */
  disabled?: boolean;
  /** 浏览器是否支持多行 CSS 遮罩（决定是否显示降级告警）。 */
  textSecuritySupported: boolean;
  /** 字段级错误（如 account_label 撞名的 inline error）。 */
  fieldError?: string | null;
}

/**
 * 单个字段的控件。
 *
 * 四种组合对应四种控件，这是 FieldSpec 的 kind / multiline 两个正交维度的直接后果：
 *   text      + 单行 → Input type="text"
 *   text      + 多行 → Textarea
 *   password  + 单行 → Input type="password"
 *   password  + 多行 → Textarea + CSS 遮罩（不支持则明文 + 告警）
 */
function FieldControl({
  spec,
  value,
  onChange,
  disabled,
  placeholder,
  textSecuritySupported,
}: {
  spec: FieldSpec;
  value: string;
  onChange: (v: string) => void;
  disabled?: boolean;
  placeholder?: string;
  textSecuritySupported: boolean;
}) {
  // checkbox：勾选框。值是字符串 "true"/"false"，与其余字段同一条管道
  // （见后端 credentialField.Kind 与 FieldSpec.kind 的注释）。
  //
  // # 为什么先判 checkbox
  //
  // 它不看 multiline / password —— 那两路的分支都是「文本输入」的变体，
  // 而 checkbox 是另一种控件。放在最前面，后面所有分支都可以假定
  // 「这是一个文本类字段」，不必再加 checkbox 排除条件。
  if (spec.kind === "checkbox") {
    return (
      <label className="flex cursor-pointer items-center gap-2">
        <input
          type="checkbox"
          className="size-4 cursor-pointer accent-primary"
          checked={value === "true"}
          disabled={disabled}
          onChange={(e) => onChange(e.target.checked ? "true" : "false")}
        />
        <span className="text-muted-foreground text-xs">
          {spec.help || "勾选后启用"}
        </span>
      </label>
    );
  }

  // password + 多行：浏览器没有「多行密码框」，textareas 不支持 type="password"。
  // 三条路都试过：明文 textarea（泄漏）、单行 password（PEM 粘不进去）、
  // textarea + CSS 遮罩（只 WebKit）。选第三条 + 显式降级。
  if (spec.kind === "password" && spec.multiline) {
    return (
      <Textarea
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        disabled={disabled}
        rows={6}
        spellCheck={false}
        // -webkit-text-security 在 Firefox 无效，所以下面还要渲染告警条。
        className={textSecuritySupported ? "font-mono text-xs [-webkit-text-security:disc]" : "font-mono text-xs"}
      />
    );
  }
  if (spec.kind === "password") {
    return (
      <Input
        type="password"
        // 用 new-password 而不是 off：Chrome/Safari 刻意忽略密码框的 off，
        // 会用「填充已保存的登录密码」取代之。new-password 是浏览器认可的
        // 「这是要新建的凭据，不是登录密码」信号。
        autoComplete="new-password"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        disabled={disabled}
      />
    );
  }
  // text + 多行：明文多行（不脱敏 —— kind 是 text 就没有脱敏语义）。
  //
  // # 这一路之前漏了
  //
  // 上面两个分支只覆盖了 password+multiline 和 password，剩下的全落进
  // 末尾的 <Input type="text">。所以 text+multiline 被渲染成单行 ——
  // 与「multiline 与 kind 正交」的设计矛盾。
  //
  // 当前没有 text+multiline 的真实字段（fosun 的两个 PEM 都是 password），
  // 所以没症状。但 meta 的 fields[].multiline 是后端给的，后端一旦给某个
  // 非敏感字段标 multiline=true（比如将来的「备注」「地址列表」），
  // 它就会被渲染成单行输入框。
  if (spec.multiline) {
    return (
      <Textarea
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        disabled={disabled}
        rows={6}
        spellCheck={false}
        className="font-mono text-xs"
      />
    );
  }
  return (
    <Input
      type="text"
      autoComplete="off"
      value={value}
      onChange={(e) => onChange(e.target.value)}
      placeholder={placeholder}
      disabled={disabled}
    />
  );
}

export function CredentialForm({
  fields,
  form,
  onChange,
  existing,
  disabled,
  textSecuritySupported,
  fieldError,
}: Props) {
  const setValue = (key: string, v: string) => {
    onChange({ ...form, values: { ...form.values, [key]: v }, clearing: { ...form.clearing, [key]: false } });
  };
  const toggleClear = (key: string) => {
    const nowClearing = !form.clearing[key];
    // 标记清除时同时清掉已输入的值 —— 否则会出现「标了清除但输入框里还有新值」
    // 的矛盾状态，提交时 buildPayload 会优先走 <clear> 而丢掉新值。
    onChange({
      ...form,
      clearing: { ...form.clearing, [key]: nowClearing },
      values: nowClearing ? { ...form.values, [key]: "" } : form.values,
    });
  };

  // 明文降级告警只针对「password + 多行」且有值的字段 ——
  // 空字段没有泄漏风险，不需要告警。
  const degradedFields = fields.filter(
    (f) => f.kind === "password" && f.multiline && !textSecuritySupported && (form.values[f.key] ?? "").trim() !== "",
  );

  return (
    <div className="grid gap-4">
      {fields.map((spec) => {
        const clearing = !!form.clearing[spec.key];
        const value = form.values[spec.key] ?? "";
        // 编辑态下 secret 字段的 placeholder 显示 hint：
        //   api_key 有 hint（用户靠它认「配的是哪一个 key」）
        //   其他 secret 只说「已保存」（多一个 hint 就是多一个泄漏面）
        const savedPlaceholder =
          existing && spec.kind === "password"
            ? spec.key === "api_key" && existing.api_key_hint
              ? `已保存（${existing.api_key_hint}），留空则不修改`
              : "已保存，留空则不修改"
            : spec.placeholder;

        return (
          <div key={spec.key} className="grid gap-1.5">
            <div className="flex items-center gap-2">
              <Label htmlFor={`cred-${spec.key}`} className="text-sm">
                {spec.label}
                {spec.required && <span className="text-destructive ml-0.5">*</span>}
              </Label>
              {/* 清除按钮的显示条件按**语义**判断，不按控件类型：
                  「有没有已保存的值可以清」。

                  语义是「清除已保存的值」而不是「清除密码」—— 所以认证字段（secret）
                  和非敏感但存了值的字段（extra，如 fosun 的 apply_account_id）
                  都该有。按 kind === "password" 判断是用实现细节代替意图，
                  会出现「语义上该有但实现上不给」的错位。
                  column 类字段（account_label / base_url）不在此列：
                  它们直接改就行，「清除」等于「改成一个空值」，没有独立语义。 */}
              {(spec.storage === "secret" || spec.storage === "extra") && existing && (
                <Button
                  type="button"
                  size="sm"
                  variant="ghost"
                  className="ml-auto h-7 px-2 text-xs"
                  disabled={disabled}
                  onClick={() => toggleClear(spec.key)}
                  /* 同一个图标，两种 tooltip —— 因为用户面对的信息不同：
                     - 密码框里是空的（不回显），「清除」= 删掉服务端存的密钥
                     - 明文框里看得到当前值，「清除」可能被误解成「清空输入框」
                       （那个用退格就行），所以要强调「保存后生效」 */
                  title={
                    spec.kind === "password"
                      ? "删除已保存的值（清除后需重新填写）；记录本身保留"
                      : "清空此字段（保存后生效）；记录本身保留"
                  }
                  aria-label={spec.kind === "password" ? "清除已保存的凭证" : "清空此字段"}
                >
                  <EraserIcon className="size-3.5" />
                  {clearing ? "已标记清空" : "清除"}
                </Button>
              )}
            </div>

            {clearing ? (
              <div className="border-destructive/40 bg-muted/40 grid gap-1.5 rounded-md border border-dashed px-3 py-2">
                <span className="text-destructive text-xs">
                  {spec.kind === "password"
                    ? "已保存的值将被清空（提交后生效）。"
                    : "该字段已保存的值将被清空（提交后生效）。"}
                </span>
                {/* # 清除态的控件复用 FieldControl，不自己写一套
                    
                    这不只是「保持一致性」这种软理由，三条硬理由：
                    
                      1. PEM 是多行内容。裸 <input> 是单行的，粘贴多行会被压成一行
                         → 用户以为填了，实际存的是半截 → 保存后解析失败且原因不明。
                      2. FieldControl 里有多行脱敏的降级处理（-webkit-text-security
                         检测 + 告警条 + 保存前二次确认）。绕过它就等于绕过这套保护。
                      3. 将来加新控件类型（如等宽 textarea）只需改 FieldControl 一处。
                    
                    你之前担心的「kind × multiline 四路分支要维护两处」——
                    在复用方案里根本不存在：那四路只在 FieldControl 里写一次。 */}
                <FieldControl
                  spec={spec}
                  value={value}
                  onChange={(v) => setValue(spec.key, v)}
                  disabled={disabled}
                  placeholder="或直接输入新值（会自动取消清除）"
                  textSecuritySupported={textSecuritySupported}
                />
                {spec.kind === "password" && (
                  <p className="text-muted-foreground text-xs">
                    输入内容已隐藏。若担心输错，可点「取消清除」回到正常模式后填写。
                  </p>
                )}
                <button
                  type="button"
                  className="text-foreground justify-self-start text-xs underline underline-offset-2"
                  onClick={() => toggleClear(spec.key)}
                >
                  取消清除
                </button>
              </div>
            ) : (
              <FieldControl
                spec={spec}
                value={value}
                onChange={(v) => setValue(spec.key, v)}
                disabled={disabled}
                placeholder={savedPlaceholder}
                textSecuritySupported={textSecuritySupported}
              />
            )}

            {spec.help && <p className="text-muted-foreground text-xs">{spec.help}</p>}

            {/* 明文降级告警必须紧贴该字段，而不是页面顶部一条 ——
                用户看到的是「这个字段现在是明文」，位置必须在字段旁边。 */}
            {spec.kind === "password" && spec.multiline && !textSecuritySupported && !clearing && (
              <p className="text-destructive text-xs font-medium">
                ⚠ 当前浏览器不支持多行脱敏显示，上面输入的内容是明文。建议改用 Chrome / Edge / Safari。
              </p>
            )}
          </div>
        );
      })}

      {/* 固定提示：不在 placeholder 里（用户一开始输入就消失）。 */}
      <p className="text-muted-foreground text-xs">
        留空 = 保留已保存的值；填写 = 替换。已保存的值永远不会回显到页面。
      </p>

      {degradedFields.length > 0 && (
        <Alert variant="destructive">
          <AlertDescription className="text-xs">
            当前浏览器（Firefox 等非 WebKit）无法遮罩多行私钥，上面已输入的私钥会明文显示。
            保存时会再次向你确认。
          </AlertDescription>
        </Alert>
      )}

      {fieldError && (
        <p className="text-destructive text-sm" role="alert">
          {fieldError}
        </p>
      )}
    </div>
  );
}
