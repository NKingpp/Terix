"use client";

import { InfoIcon } from "lucide-react";

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";

/**
 * UpdateCard 显示「自更新未启用」的说明。
 *
 * # 为什么这里不做「检查更新」，而是静态说明
 *
 * 后端已经禁用自更新（`selfupdate.Enabled = false`，`/api/update/*` 一律返回
 * 503 + `UPDATE_DISABLED`）。如果这个卡片仍然去调 `checkUpdate`，用户会看到
 * 一个「检查更新失败」的红色 toast —— 而那是**误报**：功能不是坏了，是被下掉了。
 *
 * 所以这里改成静态说明：告诉用户「不支持自更新」和「怎么升级」。
 *
 * # 为什么不只是隐藏这个卡片
 *
 * 隐藏会让「设置页没有更新入口」变成一个无法解释的缺失 ——
 * 用户可能以为是 bug 或权限问题。而明确写「不支持 + 手动升级步骤」是诚实的状态。
 *
 * # 为什么 Terix 关闭自更新
 *
 * 上游 Repo（Autumn-27/artex）是渗透测试平台。保留自更新会让用户点一次
 * 「检查更新」就下载并安装**渗透版 ARTEX 的二进制**，覆盖当前 Terix ——
 * 而 SHA256 校验会通过（SUMS 也是那个 release 的），全程无任何错误信号。
 *
 * 恢复条件：Terix 建立自己的发布源，并把 `selfupdate.Repo` 指向它。
 */
export function UpdateCard() {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <InfoIcon className="h-4 w-4" />
          自更新未启用
        </CardTitle>
        <CardDescription>
          Terix 尚未建立自己的发布源，因此不支持在应用内检查或安装更新。
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3 text-sm">
        <div>
          <p className="text-muted-foreground">升级方式（手动）：</p>
          <ol className="text-muted-foreground mt-1 list-decimal space-y-1 pl-5">
            <li>停止服务</li>
            <li>用新版本覆盖当前可执行文件</li>
            <li>重新启动</li>
          </ol>
        </div>
        <p className="text-muted-foreground border-t pt-3">
          若新版本反复启动失败，仍可用当前版本执行回滚，恢复到上一版本（回滚能力保留）。
        </p>
      </CardContent>
    </Card>
  );
}
