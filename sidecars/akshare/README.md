# AkShare sidecar

把 [akshare](https://akshare.akfamily.xyz/)（纯 Python 库）暴露成本机 HTTP 服务，
供 ARTEX（Go）调用。

## 为什么需要它

akshare 没有 HTTP 接口，ARTEX 是 Go 无法直接 import。桥接是硬需求。

**为什么用标准库 `http.server` 而不是 FastAPI**：单进程、单用户、本机回环、
QPS < 20。FastAPI 会带来 uvicorn + pydantic + starlette 三层依赖和一个异步
运行时，收益为零。`ThreadingHTTPServer`（Python 3.7+ 标准库）足够并发。

**为什么这个 Python 进程不违反 ADR-01**：ADR-01 禁的是「Python 承接 LLM 调用
与状态推进」（会绕过 `guard` 的进程内 hook）。本 sidecar **无 LLM、无状态、
无状态推进、不读 DB**，纯 `入参 → 第三方 HTTP → 出参`。这正是 ADR-01 留的窄口子。

## 数据源：新浪 + 腾讯，禁用东财

**这是实测结论，不是设计偏好。**

本机实测（2026-10）：

| akshare 函数 | 数据源 | 结果 |
|---|---|---|
| `stock_zh_a_hist` / `stock_zh_a_spot_em` / `stock_hk_hist` / `stock_us_hist` | 东方财富 | ❌ `RemoteDisconnected` |
| `stock_zh_a_daily(symbol='sh600519')` | **新浪** | ✅ 7 行 |
| `stock_zh_a_hist_tx(symbol='sh600519')` | **腾讯** | ✅ 7 行 |
| `stock_hk_daily(symbol='00700')` | **新浪** | ✅ 5499 行 |
| `stock_us_daily(symbol='AAPL')` | **新浪** | ✅ 10060 行 |
| `macro_china_gdp/cpi/pmi` | 国家统计局 | ✅ 83/225/225 行 |

**东财是持续 IP 封禁**，三重验证：
1. 带浏览器 UA + Referer 直连东财 API 同样被拒 → **封的是 IP 不是 UA**
2. 同一接口先成功一次、紧接着连续失败 → **限流触发**
3. 等待 2 分钟后重测仍失败 → **不是短时限流**

**因此代码里有硬约束 `FORBIDDEN_SOURCES`**：任何 `_em` 结尾的函数调用都会抛
`ApiError(INTERNAL)`。这是**编程错误**而非运行时数据问题 —— 若未来东财解封，
必须先重新实测确认再移除约束，不能只是"看起来好用了"就启用。

### 为什么 `/quote` 不用全市场快照

`stock_zh_a_spot()`（新浪全市场）要拉 5576 行、内部 70 次分页，实测 **18-23 秒**，
远超 §1.2 定的 8s 工具超时。为查一只股票拉全市场既浪费又超时。

**当前设计：`/quote` 复用 `/kline`**，取最后 2 根日线，close 即最新价、
前一根 close 即昨收，算涨跌幅。**1 次上游请求，实测 889ms**（比全市场扫描快 25 倍）。

**语义明确：这是"最新日线收盘价"，不是实时 tick。** 返回体带
`price_kind: "daily_close"` 显式标注，Go 侧的工具 description 也要这么写，
否则 LLM 会以为拿到的是实时价。

P0 是"日频决策 + 人工审批"，日线收盘价足够。若将来需要秒级 tick，需另接数据源。

## 安装

```bash
python -m venv .venv-akshare
.venv-akshare/Scripts/pip install -r sidecars/akshare/requirements.txt
```

**必须独立 venv**，不要复用 `.venv-fosun` —— akshare 依赖 pandas/numpy/lxml
一大堆，与复星的 `cryptography` 可能冲突。

当前 pin 版本：`akshare==1.19.3`（Python 3.13.14，pandas 3.1.0）

## 启动

```bash
.venv-akshare/Scripts/python sidecars/akshare/server.py
```

环境变量：

| 变量 | 默认 | 说明 |
|---|---|---|
| `AKSHARE_SIDECAR_ADDR` | `127.0.0.1:8791` | 监听地址。**8791 是刻意的**：8787 是 ARTEX 后端、8788 是 mitmproxy 录制代理，必须避开 |
| `AKSHARE_UPSTREAM_TIMEOUT` | `25` | 上游超时（秒），仅用于错误信息上报 |

## `/quote` 的语义边界（重要）

**返回的是「最新日线收盘价」，不是实时 tick。**

| 调用时机 | 拿到的是 |
|---|---|
| 盘中调用 | **昨日收盘价**（当日 K 线尚未收盘，不存在） |
| 盘后调用 | 当日收盘价 |
| 周末/节假日 | 上一个交易日收盘价 |

响应体里的 `price_kind: "daily_close"` 就是这个语义的显式标注，
`trading_date` 告诉你这个价属于哪一天。

**为什么不做实时 tick**：
- 东财（`_em`）实时接口在本机被 IP 封禁
- 新浪全市场快照 `stock_zh_a_spot()` 要 18-23 秒（70 次分页），远超 8s 工具超时
- P0 是「日频决策 + 人工审批」，不需要秒级行情

**需要实时价时**：要接复星（等 API Key，已有 `openapi-sdk/`）或 tushare（需 token）。
这是明确的 P0 能力缺口，不是 bug。

**误用风险**：盘中用 `/quote` 做决策会以为看到的是当前价，实际是昨收。
Go 侧的工具 description 必须写明这一点，否则 LLM 会把它当实时价用。

## 冷启动与预热

**sidecar 启动后会自动异步预热**（调用一次 kline）。预热未完成时
`/health` 返回 `"status": "warming_up"` + `"warmup_done": false`。

**Go 侧必须等 `warmup_done == true` 再发真实请求**，否则第一次调用会撞
8s 工具超时。这是「端口通了 ≠ 可用」的典型场景。

实测（本机）：
```
t=2s   /health → status=warming_up, warmup_done=false
t=24s  /health → status=ok,         warmup_done=true, warmup_elapsed_ms=802
随后   /quote  → elapsed_ms=716     ← 预热后稳定在 700-1000ms
```

**为什么预热耗时在不同机器上差异大**：pandas 的导入成本在 `import akshare`
时就已付出（进程启动阶段），所以预热本身只花 ~800ms。但如果 pandas 是
惰性导入，首次调用会到 20s+。**不要假设预热一定很快** —— 以
`warmup_done` 为准，不要以「固定等 N 秒」为准。

预热失败不致命（上游偶发不可用），`warmup_error` 会记录原因，首个真实请求会重试。

## 端点

| 端点 | 参数 | 说明 |
|---|---|---|
| `GET /health` | — | 健康检查 + **`warmup_done`**（Go 侧等它再发请求） |
| `GET /quote` | `symbol=&market=` | **最新日线收盘价**（非实时 tick），含涨跌幅 |
| `GET /kline` | `symbol=&period=&count=&market=` | K 线，多源降级 新浪→腾讯。`period`: daily/weekly/monthly；`market`: cn/hk/us |
| `GET /macro` | `indicator=cpi` | 宏观指标。`indicator`: gdp/cpi/pmi |
| `POST /_shutdown` | — | 优雅退出（测试用） |

**尚未实现**（§1.3.2 的第二批）：`/v1/news`、`/v1/breadth`、`/v1/fundamentals`。

### symbol 约定

- `cn`：6 位数字（`600519`），内部自动补 `sh`/`sz`/`bj` 前缀
  （6/9→sh，0/3→sz，4/8→bj）
- `hk`：5 位数字（`00700`）
- `us`：纯字母（`AAPL`）

### 实测响应样例

```json
// GET /quote?symbol=600519
{"ok":true,"data":{"symbol":"600519","name":"贵州茅台","last":"1349.980000",
 "open":"1325.400000","high":"1350.000000","low":"1324.790000",
 "prev_close":"1335.000000","volume":"4159916.000000","amount":"5574287630.000000"},
 "meta":{"source":"akshare","akshare_version":"1.19.3","fetched_at":"...","elapsed_ms":22971}}
```

```json
// GET /kline?symbol=600519&count=2
{"ok":true,"data":{"bars":[
  {"trading_date":"2026-10-23","open":"1311.840000","high":"1343.880000",
   "low":"1310.000000","close":"1335.000000","volume":"5768593.000000",
   "amount":"7677216283.000000","is_partial":false},
  {"trading_date":"2026-10-26","open":"1325.400000","high":"1350.000000",
   "low":"1324.790000","close":"1349.980000","volume":"4159916.000000",
   "amount":"5574287630.000000","is_partial":false}]},
 "meta":{...}}
```

## 响应格式

**价格与数量一律字符串，不是 JSON number。** 理由：JSON number 进 Go 的
`float64` 会丢精度，红线要求金额禁 float。Go 侧用 `decimal` 解析字符串，
解析失败即拒。

成功：

```json
{"ok": true, "data": {...}, "meta": {"source": "akshare", "akshare_version": "...", "fetched_at": "...", "elapsed_ms": 312}}
```

失败（**永不返回 200 + 空数组来代表失败**）：

```json
{"ok": false, "error": {"code": "AKSHARE_NO_DATA", "message": "...", "detail": {...}}}
```

## 错误码

| code | HTTP | 含义 | Go 侧是否降级 |
|---|---|---|---|
| `AKSHARE_BAD_REQUEST` | 400 | 入参校验失败 | 否（重试无意义） |
| `AKSHARE_SYMBOL_NOT_FOUND` | 404 | 标的在该源不存在 | **是**（换下一个源） |
| `AKSHARE_NO_DATA` | 200 + `ok:false` | 源可达但该区间无数据 | **是** |
| `AKSHARE_HTML_CHALLENGE` | 502 | 返回体是反爬页面 | **是**（且必须记日志，不入库） |
| `AKSHARE_UPSTREAM_TIMEOUT` | 504 | 下游超时 | **是** |
| `AKSHARE_UPSTREAM_BLOCKED` | 503 | 连接被拒（反爬）/ TLS 问题 | **是** |
| `AKSHARE_PARSE_FAILED` | 502 | 下游返回格式变了 | **是** |
| `AKSHARE_INTERNAL` | 500 | sidecar 自身 bug | **是** |

### `NO_DATA` 与 `HTML_CHALLENGE` 必须分开

前者是「今天真没数据」，后者是「源在拦我们」。混为一谈会导致**源故障被当成
无数据静默入库**，污染快照 —— PRD P0-4b 已经踩过这个坑（3 个 CSV 全是
`<!DOCTYPE` 开头的 JS challenge 页）。

本地实测的坑：新浪源对**不存在的标的**不抛异常，而是返回一个缺 `date` 列的
DataFrame（内部 `KeyError('date')`）。这在 `wrap_upstream` 里单独识别为
`SYMBOL_NOT_FOUND`，否则会被当成 `INTERNAL`，让 Go 侧白白重试。

## `is_partial` 语义

源数据 OHLC 任一为 NULL 时该 bar 仍返回，`is_partial=true`，对应字段为 `null`。
**禁止补 0** —— VIX/TNX 补 0 会让 ATR/波动率全错（已在 `KNOWN-LIMITATIONS` 之外
的共享知识里定死，见《ARCH-W2-W5》§1.3.3（docs 仓库：quantex/ARCH-W2-W5.md）；
另见《ARCH-QuantEx》§8「共享知识」的 NULL bar 条）。

## 测试

```bash
# 启动
.venv-akshare/Scripts/python sidecars/akshare/server.py &

curl -s 127.0.0.1:8791/health
curl -s "127.0.0.1:8791/quote?symbol=600519"
curl -s "127.0.0.1:8791/kline?symbol=600519&count=10"
curl -s "127.0.0.1:8791/macro?indicator=cpi"

# 关闭
curl -s -X POST 127.0.0.1:8791/_shutdown
```
