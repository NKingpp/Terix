# syntax=docker/dockerfile:1
#
# 运行镜像（不在镜像里编译）：只装常用工具，放入**预编译好的 Linux 单二进制**。
# 二进制由 CI 的 binaries job 交叉编译（纯 Go、无 QEMU），按目标架构放在
# 构建上下文的 dist/<TARGETARCH>/artex。这样多架构构建时 arm64 只需模拟 apt 层，
# 不再模拟 Next/Go 编译，速度快得多。
#
# 本地手动构建镜像时，先自行准备二进制：
#   cd web && npm run build:static && cd ..
#   cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
#   docker build -t artex:local .
FROM python:3.12-slim-bookworm
ARG TARGETARCH

# ── 国内镜像源（2026-11-15）──────────────────────────────────────────
#
# # 为什么整条链都要换源
#
# NAS 的 docker build 访问境外源会长时间无响应。表现不是报错，而是**卡住**：
# `playwright install --with-deps chromium` 要装 40+ 个 lib/fonts 包，
# apt 没有进度输出，于是构建 34 分钟没动静，看起来像死锁。
#
# 这条链上有四个互相独立的外网源，缺一个就卡：
#
#   apt        deb.debian.org            → mirrors.aliyun.com
#   Node       deb.nodesource.com        → npmmirror 的二进制 tarball
#   npm 包      registry.npmjs.org        → registry.npmmirror.com
#   chromium   cdn.playwright.dev        → cdn.npmmirror.com
#
# Node 顺手改成 tarball 而不是给 NodeSource 换镜像：少一个源，也少一次
# `curl | bash` —— 那个脚本会把境外地址写死进 sources.list.d，换源时
# 还要再拦一层，不如直接用官方二进制包。
#
# # 换源的边界
#
# 镜像源只影响**获取渠道**，不影响内容校验：apt 仍验签名（deb822 的
# Signed-By 指向镜像自带的 keyring），npm 仍验 integrity，pip 仍验 hash。
# 所以换源不是「信任降级」。
#
# 全部可用 --build-arg 覆盖回官方源，例如：
#   docker build --build-arg APT_MIRROR=https://deb.debian.org ...
ARG APT_MIRROR=https://mirrors.aliyun.com
ARG NPM_REGISTRY=https://registry.npmmirror.com
ARG NODE_MIRROR=https://cdn.npmmirror.com/binaries/node
ARG PLAYWRIGHT_DOWNLOAD_HOST=https://cdn.npmmirror.com/binaries/playwright
# bookworm 自带的 apt nodejs 是 18，Playwright 要求 >=20，所以必须自装。
ARG NODE_VERSION=20.18.1

# 换 apt 源 + 装常用工具。
#
# # 为什么要兼容两种源格式
#
# bookworm 用 deb822（/etc/apt/sources.list.d/debian.sources），但 slim 镜像
# 的版本差异会导致某些 tag 还留着传统 sources.list。两个都扫一遍。
#
# # sed 规则的顺序有讲究
#
# deb822 里 security 那条的 URI 是 deb.debian.org/**debian-security**
# （不是 security.debian.org）。若先替换短的 debian 前缀，会得到
# mirrors.aliyun.com/debian-security —— 结果碰巧正确；但若先替换成
# mirrors.aliyun.com/debian 再拼 -security，就成了
# mirrors.aliyun.com/debian/debian-security（404）。
# 显式先长后短，不依赖巧合。
RUN set -eux; \
    for f in /etc/apt/sources.list /etc/apt/sources.list.d/debian.sources; do \
      [ -f "$f" ] || continue; \
      sed -i -E \
        -e "s|https?://deb\.debian\.org/debian-security|${APT_MIRROR}/debian-security|g" \
        -e "s|https?://security\.debian\.org/debian-security|${APT_MIRROR}/debian-security|g" \
        -e "s|https?://deb\.debian\.org/debian|${APT_MIRROR}/debian|g" \
        "$f"; \
    done; \
    grep -E '^URIs:' /etc/apt/sources.list.d/debian.sources 2>/dev/null || true; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip xz-utils \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap; \
    rm -rf /var/lib/apt/lists/*

# Node 20 从二进制 tarball 装。
#
# 解到 /usr/local 是官方 node 镜像的做法：与 python 的 /usr/local
# 不冲突（python 占 bin/python* 和 lib/python3.12，node 占 bin/node*、
# lib/node_modules、include/node）。
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      amd64) node_arch=x64 ;; \
      arm64) node_arch=arm64 ;; \
      *) echo "[build] 不支持的架构: $arch（只准备了 amd64/arm64 的 Node 包）" >&2; exit 1 ;; \
    esac; \
    url="${NODE_MIRROR}/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${node_arch}.tar.xz"; \
    echo "[build] Node 包: $url"; \
    curl -fsSL --retry 3 "$url" \
      | tar -xJ -C /usr/local --strip-components=1 --no-same-owner; \
    node -v; npm -v

# 预装 Playwright MCP 与 CLI（全局），运行时不再 npx 联网下载。
# @playwright/mcp：browser MCP 直接 `npx @playwright/mcp`（已全局装好，无需 -y/@latest）。
# @playwright/cli：提供 playwright-cli，装完顺带 --help 验证可执行。
# 再装 playwright（提供浏览器管理），装完用 --with-deps 预置 chromium 及其系统依赖，
# 这样容器内 MCP/CLI 首次启动即可用，不再联网下载浏览器。
#
# PLAYWRIGHT_DOWNLOAD_HOST 只影响 chromium 的**下载地址**；浏览器二进制
# 校验仍由 playwright 自身的 sha256 完成。
# --with-deps 内部会再跑一次 apt-get update/install，用的是上面已换好的源。
RUN set -eux; \
    npm config set registry "${NPM_REGISTRY}"; \
    npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest; \
    playwright-cli --help; \
    PLAYWRIGHT_DOWNLOAD_HOST="${PLAYWRIGHT_DOWNLOAD_HOST}" playwright install --with-deps chromium; \
    rm -rf /var/lib/apt/lists/*


# 内置 akshare sidecar（与 artex 同一个容器）。
#
# # 为什么改成内置而不是独立服务
#
# 原来 sidecar 是 compose 里的第二个 service，代价是：
#   - 两套容器生命周期，排障要进两个容器；
#   - AKSHARE_SIDECAR_ADDR 在两侧语义相反（一侧目标、一侧监听），
#     配错就是全部 SIDECAR_UNREACHABLE，且错误只在运行时暴露；
#   - 每次改 server.py 都要重建一个镜像并重启另一个容器。
#
# 合进来后：一个容器、一个进程树、127.0.0.1:8791 直连，没有跨容器地址问题。
# 基础镜像本来就是 python:3.12-slim-bookworm（sidecar 也是），不额外增重。
# 代价是镜像变大（pandas/numpy 约 150MB）且升级 artex 时依赖层走缓存，不重装。
COPY sidecars/akshare/requirements.txt /app/akshare/requirements.txt
# pip index 可覆盖：默认走清华镜像。
#
# # 为什么默认不用 pypi.org（2026-11-14）
#
# 部署环境（NAS）在 docker build 网络下访问 pypi.org 会 read timeout，
# 重试 4 次后整个镜像构建失败 —— 而这次失败发生在「换二进制」时，
# 看起来像部署脚本坏了，实际是依赖层被无谓地重跑。
#
# 镜像源只是**获取渠道**，包内容与校验仍由 pip 的 hash/pin 保证，
# 所以要换源时用 --build-arg PIP_INDEX_URL=https://pypi.org/simple 即可。
#
# 实测（2026-11-14，NAS）：
#   - pypi.org 索引可达，但 wheel CDN `files.pythonhosted.org` 读超时 → 装不上
#   - 清华镜像对某些 wheel 路径返 403
#   - aliyun 镜像可用（实测 pip download akshare==1.19.3 成功）
# 要换源用 --build-arg PIP_INDEX_URL=... 覆盖。
ARG PIP_INDEX_URL=https://mirrors.aliyun.com/pypi/simple
RUN pip install --no-cache-dir --timeout 90 --retries 10 \
      -i ${PIP_INDEX_URL} -r /app/akshare/requirements.txt
COPY sidecars/akshare/server.py /app/akshare/server.py

# 守护启动脚本：进程退出后按退出码决定是否重新拉起，页面一键更新靠它完成换装。
# 它同时负责把 SIGTERM 转发给 artex —— docker stop 只把信号发给 PID 1，
# 不转发的话 artex 收不到、做不了优雅关闭，10 秒后被 SIGKILL 硬杀。
# 现在它还负责拉起 akshare sidecar 并在退出时一并回收。
COPY start.sh /app/start.sh

# 预编译好的对应架构二进制（dist/amd64/artex 或 dist/arm64/artex）。
#
# ⚠️ 这一行必须放在**所有需要联网的层之后**（下面的 akshare pip、okx CLI），
# 因为它每次部署都会变。Docker 的层缓存按「前面所有层的内容」判定 ——
# 把高频变动的产物放最后，pip/apt/npm 那些依赖层才能稳定命中缓存。
#
# # 2026-11-14 部署事故
#
# 原来这行在 pip install 之前，于是**每换一次二进制就重跑一次 pip install**。
# NAS 访问 pypi.org 会读超时（重试 4 次后失败），一次构建直接失败，
# 部署卡住，只能 docker cp 手工塞二进制绕过。
COPY dist/${TARGETARCH}/artex /app/artex
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/

# ── OKX 交易 CLI ─────────────────────────────────────────────────────
#
# # 为什么必须进镜像（2026-11-11 事故）
#
# `get_kline` 对加密标的报 `OKX_CLI_NOT_FOUND: 找不到 okx 命令`。
# 排查发现镜像里既没有 /app/bin/okx 也没有 sidecars/okx —— 而 add_company_scope
# 之外的路径全都正常，所以这个缺失只在调加密行情时才暴露。
#
# 根因：`.dockerignore` 里有 `**/node_modules`（本意是别把 web/ 的前端依赖
# 塞进构建上下文），而 OKX CLI 的本体就在 sidecars/okx/node_modules 下。
# 排掉之后 bin/okx 这个包装脚本即使 COPY 进去也是死的（它 exec 的 index.js 不存在）。
#
# 二进制 okx-auth / okx-pilot 来自 sidecars/okx/bin（git 跟踪）；
# CLI 本体（npm 包）来自 sidecars/okx/node_modules（.dockerignore 已定点放行）。
COPY bin/ /app/bin/
COPY sidecars/okx/bin/ /app/sidecars/okx/bin/
COPY sidecars/okx/node_modules/ /app/sidecars/okx/node_modules/
COPY sidecars/okx/package.json /app/sidecars/okx/package.json
RUN chmod +x /app/bin/okx /app/sidecars/okx/bin/*
# 构建期就断言 CLI 本体在：否则镜像能建成功，运行时才报 OKX_CLI_NOT_FOUND。
RUN test -f /app/sidecars/okx/node_modules/@okx_ai/okx-trade-cli/dist/index.js \
    || { echo "[build] OKX CLI 本体缺失（sidecars/okx/node_modules 未进入构建上下文，检查 .dockerignore）" >&2; exit 1; }

# data/（SQLite + jwt.key）持久化点
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
