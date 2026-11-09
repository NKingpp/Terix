// vaultd — 凭证持有进程（WE-5 骨架）。
//
// # 它为什么是独立二进制
//
// 见 exec/vaultd/server.go 的包注释：「隔离是物理的，不是约定的」。
//
// # 启动顺序（三步，缺一不可）
//
//  1. 生成 32 字节随机 token
//  2. **把 token 打印到 stdout 首行**（父进程 artex 由管道读取）
//  3. 开始监听 127.0.0.1
//
// 顺序不能换：如果先监听再打印，父进程可能在拿到 token 前就开始请求。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/Autumn-27/artex/exec/vaultd"
)

func main() {
	var (
		addr    = flag.String("addr", "127.0.0.1:0", "监听地址（端口 0 表示由内核分配）")
		verbose = flag.Bool("verbose", false, "输出 debug 日志")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	// 日志写 stderr —— stdout 的第一行必须留给 token。
	//
	// # 为什么日志不能写 stdout
	//
	// 父进程按「首行是 token」读取。如果日志混进 stdout，
	// 父进程会把一行日志当成 token，然后所有请求都 401。
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	// ---- 1. 生成 token ----
	token, err := newToken()
	if err != nil {
		slog.Error("生成 token 失败", "err", err)
		os.Exit(1)
	}

	// ---- 2. 装配通道 ----
	//
	// # 为什么凭证从环境变量读，而不是从 market_credentials 表读
	//
	// vaultd 是**独立进程**，设计上不连数据库。原因不是「懒」而是隔离：
	//   - artex 连 PG，`market_credentials` 表里躺着交易密钥的**密文/原文**。
	//     DB 一旦被读（SQL 注入、备份泄漏、误配只读账号），攻击者拿到的是
	//     「能解出密钥的材料」。
	//   - vaultd 只从环境变量拿密钥，进程内不持有任何 DB 连接。
	//     于是「读数据库」和「拿到可用交易密钥」是两件必须分别攻破的事。
	// 代价：运维要在启动 vaultd 时注入环境变量（由上层编排注入，例如 systemd
	// EnvironmentFile 或 docker secret），而不是从表里自动取 —— 这个代价换的是
	// 物理隔离，值得。
	//
	// # 变量名
	//
	//	VAULTD_OKX_API_KEY / VAULTD_OKX_API_SECRET / VAULTD_OKX_PASSPHRASE
	//	VAULTD_OKX_DEMO=true            （可选，打模拟盘）
	//	VAULTD_OKX_BASE_URL=...         （可选，测试/私有网关用）
	//	VAULTD_FOSUN_CLIENT_PRIVATE_KEY / VAULTD_FOSUN_ACCOUNT_ID
	//	VAULTD_FOSUN_SERVER_PUBLIC_KEY  （可选，当前只保存不验签）
	//	VAULTD_FOSUN_BASE_URL=...       （可选）
	channels := loadChannels()

	// 没有通道时，/v1/submit 与 /v1/query 返回 503 VAULT_CHANNEL_UNAVAILABLE，
	// 而不是 500 —— 「通道没接」是预期的部署状态，不是故障。
	srv, err := vaultd.NewServer(token, channels)
	if err != nil {
		slog.Error("构造 vaultd 失败", "err", err)
		os.Exit(1)
	}

	httpSrv, ln, err := srv.ListenAndServe(*addr)
	if err != nil {
		slog.Error("监听失败", "addr", *addr, "err", err)
		os.Exit(1)
	}

	// ---- 3. 打印 token（stdout 首行）----
	//
	// 格式：`TOKEN=<hex>`
	//
	// # 为什么带 `TOKEN=` 前缀
	//
	// 纯 hex 也能用，但加前缀让父进程的解析更明确：
	//   - 误把别的行当成 token 时，解析会失败而不是静默用错值
	//   - 人工查看时一眼知道这是什么
	fmt.Printf("TOKEN=%s\n", token)

	// 实际监听地址也打到 stderr（端口 0 时父进程需要知道真实端口）。
	//
	// # 为什么端口用 0
	//
	// 固定端口会和「本机已有服务」冲突。端口 0 让内核分配，
	// 冲突概率为零，且不会被外部扫描猜到。
	slog.Info("vaultd 已启动", "addr", ln.Addr().String())

	// 优雅关闭。
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		slog.Info("收到退出信号，关闭中")
		_ = httpSrv.Close()
	}()

	if err := httpSrv.Serve(ln); err != nil && err.Error() != "http: Server closed" {
		slog.Error("服务异常退出", "err", err)
		os.Exit(1)
	}
}

// newToken 生成 32 字节（64 hex 字符）随机 token。
//
// # 为什么是 32 字节
//
// 32 字节 = 256 bit，暴力猜测的搜索空间是 2^256 —— 在本机通信场景下
// 远超实际需要。而更长的 token 没有额外收益，只会让日志/调试更麻烦。
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// loadChannels 按环境变量装配通道，并在启动日志里打印结果。
//
// # 为什么这函数叫 load 而不是 new
//
// 它做的事是「把外部配置变成内存里的通道集合」，且**没有配置就返回空集合**
// 而不是报错 —— 「只配了 OKX」「一个都没配」都是合法部署。
//
// # 绝对不打印凭证
//
// 只打印通道名。任何 key/secret/passphrase 都不进日志，
// 包括「前四位 + 后四位」这种半脱敏形式 —— 那也是泄漏。
func loadChannels() map[string]vaultd.Channel {
	channels := map[string]vaultd.Channel{}

	// OKX 通道有两种实现，由 VAULTD_OKX_MODE 选择：
	//
	//	api（默认）—— 直连 REST API。网络环境好的机器用这个。
	//	cli        —— 调官方 `okx` 命令行工具。
	//
	// # 为什么需要 cli 模式（2026-11-06 实测）
	//
	// 本机直连 www.okx.com DNS 解析不到，直连镜像（钉 IP 后）返回 403
	// —— UA/参数/节点轮换都排除过，剩余差异是 Go 与 Node 的 TLS 指纹，
	// 镜像按指纹拦截。而同一条命令用 CLI 执行成功（CLI 内置 pilot 选节点）。
	//
	// 所以 cli 模式不是「更方便」，而是「在当前网络下唯一可用」。
	okxMode := strings.ToLower(strings.TrimSpace(os.Getenv("VAULTD_OKX_MODE")))
	if okxMode == "" {
		okxMode = "api"
	}
	switch okxMode {
	case "cli":
		if ch, err := vaultd.NewOKXCLIChannel(vaultd.OKXCLIConfig{
			APIKey:     os.Getenv("VAULTD_OKX_API_KEY"),
			SecretKey:  os.Getenv("VAULTD_OKX_API_SECRET"),
			Passphrase: os.Getenv("VAULTD_OKX_PASSPHRASE"),
			Demo:       truthy(os.Getenv("VAULTD_OKX_DEMO")),
			BinPath:    os.Getenv("VAULTD_OKX_BIN"),
		}); err != nil {
			slog.Warn("OKX 通道未加载(cli)", "reason", err.Error())
		} else {
			channels[vaultd.ChannelOKX] = ch
			slog.Info("OKX 通道使用 cli 模式")
		}
	case "api":
		if ch, err := vaultd.NewOKXChannel(vaultd.OKXConfig{
			APIKey:      os.Getenv("VAULTD_OKX_API_KEY"),
			SecretKey:   os.Getenv("VAULTD_OKX_API_SECRET"),
			Passphrase:  os.Getenv("VAULTD_OKX_PASSPHRASE"),
			BaseURL:     os.Getenv("VAULTD_OKX_BASE_URL"),
			Demo:        truthy(os.Getenv("VAULTD_OKX_DEMO")),
			ResolveHost: os.Getenv("VAULTD_OKX_RESOLVE_HOST"),
			ResolveIP:   os.Getenv("VAULTD_OKX_RESOLVE_IP"),
		}); err != nil {
			// 注意：err 里只有「哪个字段为空」，不含值。
			slog.Warn("OKX 通道未加载(api)", "reason", err.Error())
		} else {
			channels[vaultd.ChannelOKX] = ch
		}
	default:
		slog.Warn("VAULTD_OKX_MODE 无效，已跳过 OKX 通道", "mode", okxMode)
	}

	if ch, err := vaultd.NewFosunChannel(vaultd.FosunConfig{
		ServerPublicKey:  os.Getenv("VAULTD_FOSUN_SERVER_PUBLIC_KEY"),
		ClientPrivateKey: os.Getenv("VAULTD_FOSUN_CLIENT_PRIVATE_KEY"),
		AccountID:        os.Getenv("VAULTD_FOSUN_ACCOUNT_ID"),
		BaseURL:          os.Getenv("VAULTD_FOSUN_BASE_URL"),
	}); err != nil {
		slog.Warn("复星通道未加载", "reason", err.Error())
	} else {
		slog.Warn("复星通道已装配但**未接入验证**（缺官方文档，不可用于真实下单）")
		channels[vaultd.ChannelFosun] = ch
	}

	// 打印一行摘要，让运维一眼看到状态（而不是去猜为什么 /v1/submit 返回 503）。
	if len(channels) == 0 {
		slog.Warn("无通道 —— /v1/submit 与 /v1/query 将返回 503（预期部署状态，非故障）")
		return channels
	}
	names := make([]string, 0, len(channels))
	for k := range channels {
		names = append(names, k)
	}
	sort.Strings(names)
	slog.Info("已加载通道", "channels", strings.Join(names, ","))
	return channels
}

// truthy 解析布尔环境变量（宽松一点：部署脚本里常写 1/true/yes）。
func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
