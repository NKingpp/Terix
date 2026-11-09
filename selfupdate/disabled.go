package selfupdate

// Enabled 表示自更新功能是否启用。
//
// # 为什么 Terix 关闭自更新
//
// 上游 Repo（Autumn-27/artex）是「AI 渗透测试平台」。
// 如果保留自更新，用户点「检查更新」会：
//
//	读 Autumn-27/artex 的 latest release
//	→ 下载该 release 的二进制
//	→ SHA256 校验**通过**（因为 SUMS 也是那个 release 的）
//	→ 原子替换自己
//	→ 重启后是渗透版 ARTEX
//
// 这不是「可能出错」，是「一定会换种」。
//
// # 为什么不改 Repo 指向 Terix 的仓库
//
// Terix 没有发布源。改成一个不存在的仓库会让「检查更新」永远 404，
// 用户看到「检查失败」却不知道是「这个功能被下掉了」——
// 那是比「明确不支持」更糟的体验。
//
// # 为什么保留代码而不删
//
// Bootstrap() 还负责「新版本反复崩溃时回滚到旧版本」，以及
// Rollback() 手动回滚 —— 那个能力在运维手动替换二进制时仍然有用。
//
// 所以分界是：
//
//	拉取/下载/应用（CheckLatest / FetchLatest / Stage / updateApply）→ 禁用
//	引导/回滚（Bootstrap / Settle / Rollback / HasBackup）        → 保留
//
// # 何时恢复
//
// Terix 建立自己的发布源后：把 Enabled 改成 true，**并同时**把 Repo 指向自己的仓库。
//
// ⚠️ 两件事必须同时做 —— 只改 Enabled 会立刻回到「换种」隐患。
// 由 TestSelfUpdateDisabledByDefault 守护这个前提。
const Enabled = false

// ErrDisabled 是自更新被禁用时返回的哨兵错误。
//
// # 为什么用哨兵而不是普通 error
//
// 调用方（handler）需要区分「功能被下掉」和「网络失败」——
// 前者应返回 503 + 明确的「未启用」，后者才是 502。
// 如果都用普通 error，调用方只能匹配字符串，那是脆弱的。
var ErrDisabled = errDisabled{}

type errDisabled struct{}

func (errDisabled) Error() string {
	return "自更新功能未启用（Terix 尚未建立发布源）。" +
		"升级请手动替换二进制：停服务 → 覆盖可执行文件 → 启动"
}

// repoMismatch 说明当前 Repo 常量指向的不是 Terix 自己的仓库。
//
// # 为什么单独一个函数而不是常量
//
// 它只用于测试与诊断输出 —— 让「为什么禁用」在运行时可查，
// 而不只是藏在注释里。
func repoMismatch() bool {
	// Terix 的合法发布源应为 "..." 形式，而当前是上游渗透版仓库。
	// 这里不做字符串匹配（那会在改 Repo 时忘记改这里），
	// 而是依赖 Enabled 这个显式开关 —— 本函数的唯一作用是
	// 给测试和日志提供「当前 Repo 是否还是上游」的旁证。
	return Repo == "Autumn-27/artex"
}
