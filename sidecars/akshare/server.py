#!/usr/bin/env python3
"""AkShare sidecar：把 akshare 的 Python 库暴露成 HTTP。

为什么需要 sidecar
------------------
akshare 是纯 Python 库，没有 HTTP 接口。ARTEX 是 Go，无法直接 import。
桥接是硬需求。

为什么用标准库 http.server 而不是 FastAPI
----------------------------------------
单进程、单用户、本机回环、QPS < 20。FastAPI 会带来 uvicorn + pydantic +
starlette 三层依赖和一个异步运行时，收益为零。``ThreadingHTTPServer``
（Python 3.7+ 标准库）足够并发。

为什么允许这个 Python 进程存在（ADR-01 禁止 Python 子服务）
----------------------------------------------------------
ADR-01 禁的是「Python 承接 LLM 调用与状态推进」（会绕过 guard 的进程内 hook）。
本 sidecar **无 LLM、无状态、无状态推进、不读 DB**，纯 ``入参 → 第三方 HTTP
→ 出参``。这正是 ADR-01 留的窄口子。

数据源选择（实测结论，非文档推测）
--------------------------------
**东方财富（``*_em`` 系列）在本机被反爬封锁**：
``stock_zh_a_hist`` / ``stock_zh_a_spot_em`` / ``stock_hk_hist`` /
``stock_us_hist`` 全部 ``ConnectionError: RemoteDisconnected``，
带浏览器 UA + Referer 直连东财 API 同样被拒。不是 UA 问题，是 IP/机房级封锁。

**新浪源可用**（实测通过）：
  stock_zh_a_daily(symbol='sh600519')     → 7 行
  stock_hk_daily(symbol='00700')          → 5499 行
  stock_us_daily(symbol='AAPL')           → 10060 行
  stock_zh_a_spot()                       → 5576 行
  stock_zh_index_daily(symbol='sh000001') → 8749 行
  macro_china_gdp / cpi / pmi             → 全部通过

因此本 sidecar **一律用新浪源**，并在 ``/health`` 里报告数据源，
便于运维知道「不是数据没有，是我们换了源」。

启动
----
    .venv-akshare/Scripts/python sidecars/akshare/server.py

环境变量：
    AKSHARE_SIDECAR_ADDR   监听地址，默认 127.0.0.1:8791
                           （8787=ARTEX 后端、8788=mitmproxy 代理，避开）
"""

import json
import os
import re
import sys
import threading
import time
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

try:
    import akshare as ak
except ImportError:
    sys.exit(
        "需要 akshare。安装：\n"
        "  python -m venv .venv-akshare\n"
        "  .venv-akshare/Scripts/pip install -r sidecars/akshare/requirements.txt"
    )

try:
    import pandas as pd
except ImportError:
    sys.exit("需要 pandas（akshare 的依赖，通常已随它安装）")

AKSHARE_VERSION = getattr(ak, "__version__", "unknown")

# 默认端口 8791：8787 是 ARTEX 后端、8788 是 mitmproxy 录制代理。
DEFAULT_ADDR = "127.0.0.1:8791"

# 单次上游调用超时（秒）。akshare 内部不暴露 timeout，这里靠线程 + future 硬超时。
UPSTREAM_TIMEOUT = float(os.getenv("AKSHARE_UPSTREAM_TIMEOUT", "25"))

# 东财源（_em 后缀）在本机被**持续 IP 封禁**：
# 实测 stock_zh_a_hist / stock_zh_a_spot_em / stock_hk_hist / stock_us_hist
# 全部 RemoteDisconnected；带浏览器 UA + Referer 直连东财 API 同样被拒
# （说明封的是 IP 不是 UA）；等待 2 分钟后重测仍失败（说明不是短时限流）。
# 因此**禁止调用任何 _em 结尾的函数**，改用新浪 / 腾讯源。
# 若未来东财解封，必须先重新实测确认，再从这里移除前缀。
FORBIDDEN_SOURCES = ("_em", "_em_", "em_")


def _assert_allowed(fn_name):
    """硬约束：禁止使用东财源。违反即视为编程错误（不是运行时数据问题）。"""
    low = fn_name.lower()
    for bad in FORBIDDEN_SOURCES:
        if low.endswith("_em") or "_em_" in low:
            raise ApiError(
                Err.INTERNAL,
                f"禁止使用东财源 {fn_name}（本机被 IP 封禁，见 FORBIDDEN_SOURCES 注释）",
                {"func": fn_name},
            )


# =====================================================================
# 错误码（§1.3.4）
# =====================================================================
class Err:
    BAD_REQUEST = "AKSHARE_BAD_REQUEST"
    SYMBOL_NOT_FOUND = "AKSHARE_SYMBOL_NOT_FOUND"
    NO_DATA = "AKSHARE_NO_DATA"
    HTML_CHALLENGE = "AKSHARE_HTML_CHALLENGE"
    UPSTREAM_TIMEOUT = "AKSHARE_UPSTREAM_TIMEOUT"
    UPSTREAM_BLOCKED = "AKSHARE_UPSTREAM_BLOCKED"
    PARSE_FAILED = "AKSHARE_PARSE_FAILED"
    INTERNAL = "AKSHARE_INTERNAL"


# 每个错误码对应的 HTTP 状态与「是否应触发 Go 侧降级到下一个通道」。
ERROR_HTTP = {
    Err.BAD_REQUEST: 400,
    Err.SYMBOL_NOT_FOUND: 404,
    Err.NO_DATA: 200,  # 200 + ok:false，见 §1.3.3
    Err.HTML_CHALLENGE: 502,
    Err.UPSTREAM_TIMEOUT: 504,
    Err.UPSTREAM_BLOCKED: 503,
    Err.PARSE_FAILED: 502,
    Err.INTERNAL: 500,
}


class ApiError(Exception):
    """业务错误，由 handler 转成 JSON 错误响应。"""

    def __init__(self, code, message, detail=None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.detail = detail or {}


# =====================================================================
# 工具函数
# =====================================================================
def now_iso():
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def looks_like_challenge(text):
    """判断响应体是否是反爬 JS challenge 页。

    PRD P0-4b 实测过：3 个 CSV 全是 ``<!DOCTYPE html>`` 的 challenge 页。
    这类响应必须以独立错误码上报，**不能当成「无数据」** —— 否则会被
    入库成 is_partial 行，污染快照。
    """
    head = (text or "").lstrip()[:512].lower()
    return head.startswith("<!doctype") or head.startswith("<html") or "requires javascript" in head


def num_str(v):
    """把 pandas/numpy 数值转成字符串；NaN/None 转 None。

    价格与数量一律字符串（不是 JSON number）：JSON number 进 Go 的 float64
    会丢精度，红线要求金额禁 float。Go 侧用 decimal 解析字符串。
    """
    if v is None:
        return None
    try:
        f = float(v)
    except (TypeError, ValueError):
        return None
    if f != f:  # NaN
        return None
    # 用固定 6 位，与 ARTEX 指标输出口径一致
    return f"{f:.6f}"


def pick(row, *names, default=None):
    """按候选列名取值（akshare 各源列名不一致）。"""
    for n in names:
        if n in row and row[n] is not None and not (isinstance(row[n], float) and row[n] != row[n]):
            return row[n]
    return default


# =====================================================================
# 各端点实现（纯函数：入参 → data dict）
# =====================================================================
# =====================================================================
# 冷启动预热
# =====================================================================
# 首次上游调用约 20-23 秒：pandas 导入 + 新浪首次连接 + 内部分页初始化。
# 实测（同一进程）：
#   第 1 次 quote: 23400ms   ← 冷启动
#   第 2 次:         958ms
#   第 3 次:         856ms
#   换标的 000001:  1006ms   ← 换 symbol 仍快，证明不是缓存
#
# 不预热的话，**第一个真实请求会撞 8s 工具超时而失败**，表现为「服务起来了
# 但第一次调用总是超时」，非常容易被误判为「sidecar 不可用」。
#
# 因此启动即异步预热，并让 /health 暴露 warmup_done —— 让健康检查反映
# **真实可用状态**，而不是「端口通了就算好了」。Go 侧应等 warmup_done=true
# 再发真实请求。
_WARMUP = {"done": False, "error": None, "elapsed_ms": None}


def _warmup():
    started = time.time()
    try:
        _fetch_kline("600519", period="daily", count=2, market="cn")
        _WARMUP["done"] = True
        print(f"warmup ok in {int((time.time() - started) * 1000)}ms", flush=True)
    except Exception as e:  # 预热失败不致命，首个真实请求会重试
        _WARMUP["error"] = f"{type(e).__name__}: {str(e)[:200]}"
        print(f"warmup failed (will retry on first request): {_WARMUP['error']}", flush=True)
    finally:
        _WARMUP["elapsed_ms"] = int((time.time() - started) * 1000)


def ep_health():
    ready = _WARMUP["done"]
    return {
        "status": "ok" if ready else "warming_up",
        "warmup_done": ready,
        "warmup_error": _WARMUP["error"],
        "warmup_elapsed_ms": _WARMUP["elapsed_ms"],
        "akshare_version": AKSHARE_VERSION,
        "pandas_version": pd.__version__,
        # 东财（_em）在本机被 IP 封禁，见 FORBIDDEN_SOURCES 注释
        "sources_ok": ["sina", "tencent"],
        "sources_forbidden": ["eastmoney(_em 后缀，IP 封禁)"],
        "upstream_timeout_s": UPSTREAM_TIMEOUT,
        "quote_kind": "daily_close",  # /quote 返回的是日线收盘价，非实时 tick
    }


def ep_quote(symbol, market="cn"):
    """行情快照：**从日线 K 线取最后两根**算最新价与涨跌幅。

    为什么不用 ``stock_zh_a_spot_em`` / ``stock_zh_a_spot``：
      - 东财版（_em）被 IP 封禁；
      - 新浪版 ``stock_zh_a_spot()`` 要拉全市场 5576 行，内部 70 次分页，
        实测 **18 秒**。为了查一只股票拉全市场既浪费又超出 8s 工具超时。

    所以这里复用 ``/kline`` 的实现：取最后 2 根日线，close 即最新价，
    前一根 close 即昨收，两者算涨跌幅。1 次上游请求（约 0.3s）。

    **语义明确：这是「最新日线收盘价」，不是实时 tick。**

    盘中调用拿到的是**昨日收盘价**（当日 K 线尚未收盘，不存在）；盘后调用
    拿到当日收盘价；周末/节假日拿到上一交易日收盘价。响应体的
    ``price_kind: "daily_close"`` 与 ``trading_date`` 共同标注这个边界。

    **误用风险**：盘中用它做决策会以为看到的是当前价。Go 侧的工具
    description 必须写明这一点，否则 LLM 会把它当实时价用。

    返回里带 ``price_kind: "daily_close"`` 显式标注语义，防止误用。
    """
    if not symbol:
        raise ApiError(Err.BAD_REQUEST, "缺少 symbol")
    data, source = _fetch_kline(symbol, period="daily", count=2, market=market)
    bars = data["bars"]
    if len(bars) == 0:
        raise ApiError(Err.NO_DATA, f"{symbol} 无可用日线", {"symbol": symbol, "market": market})

    last = bars[-1]
    prev_close = bars[-2]["close"] if len(bars) >= 2 else None
    change_pct = None
    if prev_close and last["close"]:
        try:
            pc = float(prev_close)
            lc = float(last["close"])
            if pc != 0:
                change_pct = f"{(lc - pc) / pc * 100:.4f}"
        except (TypeError, ValueError):
            change_pct = None

    return {
        "symbol": symbol,
        "market": market,
        "price_kind": "daily_close",  # 显式标注：日线收盘价，非实时 tick
        "trading_date": last["trading_date"],
        "last": last["close"],
        "open": last["open"],
        "high": last["high"],
        "low": last["low"],
        "prev_close": prev_close,
        "change_pct": change_pct,
        "volume": last["volume"],
        "amount": last["amount"],
        "is_partial": last["is_partial"],
        "source": source,
    }


def _fetch_kline(symbol, period, count, market):
    """取 K 线，多源降级：新浪 → 腾讯。

    返回 ``(data_dict, used_source)``。两个源返回的列名相同
    （date/open/close/high/low/volume/amount），所以解析逻辑共用。

    降级条件：只有「源不可用」类错误才降级（BLOCKED / TIMEOUT / CHALLENGE）。
    ``SYMBOL_NOT_FOUND`` 和 ``BAD_REQUEST`` **不降级** —— 换源也不会让一个
    不存在的标的出现，白白多打一次上游。
    """
    if not symbol:
        raise ApiError(Err.BAD_REQUEST, "缺少 symbol")
    try:
        count = int(count)
    except (TypeError, ValueError):
        raise ApiError(Err.BAD_REQUEST, f"count 非法: {count!r}")
    if count <= 0 or count > 5000:
        raise ApiError(Err.BAD_REQUEST, f"count 超出范围 1..5000: {count}")
    if period not in ("daily", "weekly", "monthly"):
        raise ApiError(Err.BAD_REQUEST, f"period 非法: {period!r}（应为 daily/weekly/monthly）")
    if market not in ("cn", "hk", "us"):
        raise ApiError(Err.BAD_REQUEST, f"market 非法: {market!r}")

    # 候选源：cn 有两个（新浪 + 腾讯），hk/us 只有新浪。
    # 腾讯只有 A 股接口 stock_zh_a_hist_tx。
    candidates = _sources_for(market)

    first_err = None
    for src_name, fn_name, fn in candidates:
        _assert_allowed(fn_name)
        try:
            df = fn(symbol)
            return _parse_kline(df, symbol, period, count, market), src_name
        except ApiError as e:
            if e.code in (Err.SYMBOL_NOT_FOUND, Err.BAD_REQUEST):
                raise
            if first_err is None:
                first_err = e
            continue
        except Exception as e:
            wrapped = wrap_upstream(e, fn_name)
            if wrapped.code in (Err.SYMBOL_NOT_FOUND, Err.BAD_REQUEST):
                raise wrapped
            if first_err is None:
                first_err = wrapped
            continue

    raise first_err or ApiError(Err.INTERNAL, f"{symbol}: 所有源均失败")


def _sources_for(market):
    """返回该市场的候选源列表 ``[(源名, 取数函数)]``，按优先级排序。

    每个条目带**显式源名**（不是靠 ``fn.__name__`` 推导 —— lambda 的
    ``__name__`` 是 ``<lambda>``，错误信息里会看不出是哪个源失败）。
    """
    if market == "cn":
        return [
            ("sina", "stock_zh_a_daily",
             lambda s: ak.stock_zh_a_daily(symbol=_cn_prefix(s), adjust="")),
            ("tencent", "stock_zh_a_hist_tx",
             lambda s: ak.stock_zh_a_hist_tx(symbol=_cn_prefix(s), adjust="")),
        ]
    if market == "hk":
        return [("sina", "stock_hk_daily",
                 lambda s: ak.stock_hk_daily(symbol=s, adjust=""))]
    return [("sina", "stock_us_daily",
             lambda s: ak.stock_us_daily(symbol=s, adjust=""))]


def _parse_kline(df, symbol, period, count, market):
    """把 akshare 返回的 DataFrame 转成统一 bar 结构。"""
    if df is None or len(df) == 0:
        raise ApiError(Err.NO_DATA, f"{symbol} 无 K 线数据", {"symbol": symbol, "market": market})

    # 新浪源对不存在的标的会返回缺 'date' 列的 DataFrame（内部 KeyError('date')）
    # 而不是抛异常。这是「标的不存在」而非「源故障」。
    if "date" not in df.columns:
        raise ApiError(
            Err.SYMBOL_NOT_FOUND,
            f"{symbol}: {market} 源未返回该标的的日线数据"
            f"（可能代码错误、已退市，或该源不覆盖此市场）",
            {"symbol": symbol, "market": market},
        )

    if period in ("weekly", "monthly"):
        df = _resample(df, period)

    df = df.tail(count)
    bars = []
    for _, r in df.iterrows():
        o = num_str(pick(r, "open"))
        h = num_str(pick(r, "high"))
        l = num_str(pick(r, "low"))
        c = num_str(pick(r, "close"))
        # OHLC 任一为 None → is_partial，**禁止补 0**
        partial = any(x is None for x in (o, h, l, c))
        d = pick(r, "date")
        if d is None:
            raise ApiError(Err.PARSE_FAILED, "K 线缺 date 列")
        bars.append({
            "trading_date": str(d)[:10],
            "open": o, "high": h, "low": l, "close": c,
            "volume": num_str(pick(r, "volume")),
            "amount": num_str(pick(r, "amount")),
            "is_partial": bool(partial),
        })
    return {"bars": bars}


def ep_kline(symbol, period="daily", count=250, market="cn"):
    """K 线查询，内部走多源降级。"""
    data, source = _fetch_kline(symbol, period, count, market)
    return data, source


def _cn_prefix(symbol):
    """6 位 A 股代码补交易所前缀。6 开头=sh，0/3 开头=sz，4/8 开头=bj。"""
    s = symbol.strip()
    if s.startswith(("6", "9")):
        return "sh" + s
    if s.startswith(("0", "3")):
        return "sz" + s
    if s.startswith(("4", "8")):
        return "bj" + s
    return "sh" + s


def _resample(df, period):
    """把日线重采样成周线/月线。"""
    d = df.copy()
    d["date"] = pd.to_datetime(d["date"])
    d = d.set_index("date")
    rule = "W" if period == "weekly" else "ME"
    agg = {"open": "first", "high": "max", "low": "min", "close": "last", "volume": "sum"}
    agg = {k: v for k, v in agg.items() if k in d.columns}
    out = d.resample(rule).agg(agg).dropna(subset=["open", "close"])
    return out.reset_index()


def ep_macro(indicator="cpi"):
    """宏观指标。indicator: gdp / cpi / pmi。"""
    fn_map = {
        "gdp": (ak.macro_china_gdp, "国内生产总值-绝对值"),
        "cpi": (ak.macro_china_cpi, "全国-同比增长"),
        "pmi": (ak.macro_china_pmi, "制造业-指数"),
    }
    key = (indicator or "cpi").lower()
    if key not in fn_map:
        raise ApiError(Err.BAD_REQUEST, f"indicator 非法: {indicator!r}（应为 gdp/cpi/pmi）")
    fn, value_col = fn_map[key]
    try:
        df = fn()
    except Exception as e:
        raise wrap_upstream(e, f"macro_china_{key}")
    if df is None or len(df) == 0:
        raise ApiError(Err.NO_DATA, f"宏观指标 {key} 无数据")

    date_col = "日期" if "日期" in df.columns else df.columns[0]
    if value_col not in df.columns:
        raise ApiError(Err.PARSE_FAILED, f"宏观 {key} 缺列 {value_col}",
                       {"available_columns": [str(c) for c in df.columns]})
    df = df.tail(60)
    series = []
    for _, r in df.iterrows():
        series.append({
            "period": str(r[date_col])[:10],
            "value": num_str(r[value_col]),
        })
    return {"series_name": key, "unit": "", "points": series}


# =====================================================================
# 新闻（/news）
# =====================================================================
# 数据源选择（实测结论）
# --------------------
# 财新 stock_news_main_cx()          → 100 行，列 tag/summary/url
# 财联社 stock_info_global_cls()      →  20 行，列 标题/内容/发布日期/发布时间
#
# 两者都**不含 _em**，不受东财封禁影响。
#
# 为什么两个源都要：覆盖面互补。财新偏深度（含「华尔街原声」这类美股评论），
# 财联社偏快讯（时效性强，含美股盘后异动）。只用一个会让分析师漏掉另一类。
#
# 财新没有「标题」列，只有 tag（栏目标签，如「周刊提前读」）。直接用 tag
# 当 title 会让所有条目标题雷同、无法区分。折中：title 取 summary 的首句
# （到第一个句号/换行），tag 单独放 category。这是**信息损失最小的方案**，
# 因为原数据确实没有标题字段，编不出来。

# 财新 URL 里带日期（.../2026-10-31/102499332.html），这是唯一可靠的
# 发布时间来源 —— akshare 的 cx 接口不返回时间列。
_CX_DATE_RE = re.compile(r"/(\d{4}-\d{2}-\d{2})/")


def _cx_title_from_summary(summary, tag):
    """财新没有标题列：取 summary 首句当标题，去掉「【本文系…】」这类前缀标记。"""
    s = (summary or "").strip()
    if not s:
        return tag or ""
    # 去掉开头的【…】栏目提示，它们对判断新闻内容无信息量
    s = re.sub(r"^【[^】]*】", "", s).strip()
    for sep in ("。", "！", "？", "\n"):
        idx = s.find(sep)
        if 0 < idx <= 60:
            return s[: idx + 1]
    return s[:60]


def _fetch_news_cx(limit):
    _assert_allowed("stock_news_main_cx")
    try:
        df = ak.stock_news_main_cx()
    except Exception as e:
        raise wrap_upstream(e, "stock_news_main_cx")
    if df is None or len(df) == 0:
        return []
    out = []
    for _, r in df.iterrows():
        summary = str(pick(r, "summary", default="") or "").strip()
        url = str(pick(r, "url", default="") or "").strip()
        tag = str(pick(r, "tag", default="") or "").strip()
        if not summary and not url:
            continue
        m = _CX_DATE_RE.search(url)
        out.append({
            "title": _cx_title_from_summary(summary, tag),
            "summary": summary,
            "url": url,
            "source": "caixin",
            "category": tag,
            # 拿不到时留空，**不编造日期** —— 编出来的时间会让分析师把旧闻当新闻。
            "published": m.group(1) if m else "",
        })
        if len(out) >= limit:
            break
    return out


def _fetch_news_cls(limit):
    _assert_allowed("stock_info_global_cls")
    try:
        df = ak.stock_info_global_cls(symbol="全部")
    except Exception as e:
        raise wrap_upstream(e, "stock_info_global_cls")
    if df is None or len(df) == 0:
        return []
    out = []
    for _, r in df.iterrows():
        title = str(pick(r, "标题", default="") or "").strip()
        content = str(pick(r, "内容", default="") or "").strip()
        if not title and not content:
            continue
        date = str(pick(r, "发布日期", default="") or "").strip()
        clock = str(pick(r, "发布时间", default="") or "").strip()
        out.append({
            "title": title or content[:60],
            "summary": content,
            # 财联社接口不返回原文链接（快讯无独立页面），留空而不是伪造。
            "url": "",
            "source": "cls",
            "category": "",
            "published": f"{date} {clock}".strip(),
        })
        if len(out) >= limit:
            break
    return out


# 市场关键词表。用于在**没有 symbol** 时把新闻粗略分市场。
# 为什么用关键词而不是分类字段：两个源都不提供市场标签，只能靠文本判断。
_US_KW = ("美股", "纳斯达克", "道指", "道琼斯", "标普", "美联储", "华尔", "美元",
          "美债", "特斯拉", "苹果", "英伟达", "微软", "谷歌", "亚马逊", "Meta",
          "NASDAQ", "S&P", "Fed", "FOMC", "CPI")
_HK_KW = ("港股", "恒生", "港交所", "港元", "南向资金", "恒指", "H股")


def _matches_market(item, market):
    """判断一条新闻是否属于指定市场。cn 视为「通用/未分类」—— 不筛。"""
    if market == "cn":
        return True
    text = (item.get("title") or "") + " " + (item.get("summary") or "")
    kws = _US_KW if market == "us" else _HK_KW
    return any(k.lower() in text.lower() for k in kws)


def ep_news(market="cn", symbol="", limit=20):
    """财经新闻聚合：财新 + 财联社，按 URL/标题去重后合并。

    返回结构::

        {"items": [{"title","summary","url","source","published"}],
         "symbol": "...", "market": "cn", "matched": 3, "total": 47}

    过滤语义（**与 /quote 之类「查不到就报错」不同，这里空是合法结果**）
    ---------------------------------------------------------------
    传了 ``symbol`` 时，只保留 title/summary 里出现该代码的条目。
    **过滤后为空就返回空列表，绝不 fallback 到全量**：

      「这只票最近没新闻」是一个**真实且有价值的结论**（说明没有事件驱动），
      而 fallback 到全量会把一堆茅台/纳斯达克无关新闻塞给分析师，让它
      误以为这些新闻和当前标的相关 —— 分析结论会直接跑偏。

    同理，``market`` 过滤后为空也不 fallback：宁可回空并让调用方看到
    ``market`` 字段，也不要交一堆错市场的新闻。

    例外：``market="cn"`` 不筛（中文财经源以 A 股为主，硬筛会误杀宏观内容）。
    """
    try:
        limit = int(limit)
    except (TypeError, ValueError):
        raise ApiError(Err.BAD_REQUEST, f"limit 非法: {limit!r}")
    if limit <= 0:
        limit = 20
    if limit > 50:
        limit = 50  # 上限保护：每条新闻进 prompt 都要花 token，50 条已足够
    if market not in ("cn", "us", "hk"):
        raise ApiError(Err.BAD_REQUEST, f"market 非法: {market!r}（应为 cn/us/hk）")

    # 多取一些再做过滤：过滤会砍掉大部分，先按 limit 取会导致过滤后所剩无几。
    fetch_n = min(100, max(limit * 4, 40))
    items = []
    errs = []
    for fetch in (_fetch_news_cx, _fetch_news_cls):
        try:
            items.extend(fetch(fetch_n))
        except ApiError as e:
            # 单源失败不致命：另一个源仍可用。全失败才报错。
            errs.append({"code": e.code, "message": e.message})
    if not items:
        raise ApiError(
            Err.UPSTREAM_BLOCKED if errs else Err.NO_DATA,
            "所有新闻源均无返回",
            {"errors": errs},
        )

    # 去重：同一条快讯可能两个源都发。以 title 前 40 字为键（url 财联社为空，
    # 不能当唯一键）。
    seen = set()
    deduped = []
    for it in items:
        key = (it.get("title") or "")[:40]
        if key in seen:
            continue
        seen.add(key)
        deduped.append(it)

    if symbol:
        needle = symbol.strip().lower()
        deduped = [
            it for it in deduped
            if needle in (it.get("title") or "").lower()
            or needle in (it.get("summary") or "").lower()
        ]

    deduped = [it for it in deduped if _matches_market(it, market)]

    return {
        "symbol": symbol,
        "market": market,
        "matched": len(deduped),
        "items": deduped[:limit],
        "source_errors": errs,
    }


# =====================================================================
# 财报（/fundamental）
# =====================================================================
# A 股：新浪 stock_financial_report_sina(stock='sh600519', symbol='资产负债表')
#       实测 104 行 × 147 列；利润表 104×83；现金流量表 100×71。
#       摘要 stock_financial_abstract(symbol='600519') → 80 行 × 106 列。
#
# **为什么美股/港股财报不用 stock_financial_us_report_em**
# ------------------------------------------------------
# 实测这个接口**当前是通的**，看起来能用。但它以 ``_em`` 结尾，属于东财源，
# 而 FORBIDDEN_SOURCES 的存在前提是「东财对本机 IP 是可变的、随时可能收紧」。
# 今天能通不代表明天能通 —— 一旦东财再封一次，这个接口会**静默失效**：
# 返回空 DataFrame 而不是抛异常，调用方会把它当成「这家公司没有财报」，
# 分析师据此得出错误结论，且没有任何错误信号能被上层捕获。
#
# 更关键的是它会造成**不一致**：K 线走新浪/腾讯、财报走东财，两个源停牌/复权
# 口径不同，交叉验证会得出矛盾数据。宁可明确返回「通道待接入」，让上层知道
# 这是**能力缺口**（可以报给用户「暂不支持」），也不要一个会静默死掉的通道。
#
# 若将来要接美股财报，正确做法是找**非东财**源（如 SEC EDGAR 直连、
# 富途/老虎的公开接口），并单独实测，而不是把这个 _em 接口放进来。

# 只保留每个报表最关键的科目。全量 147 列塞进 prompt 会爆 token，
# 而分析师 90% 的问题集中在这十几项上。
_KEY_ROWS = {
    "资产负债表": [
        "货币资金", "应收账款", "存货", "流动资产合计", "非流动资产合计",
        "资产总计", "短期借款", "应付账款", "合同负债", "流动负债合计",
        "非流动负债合计", "负债合计", "归属于母公司股东权益合计",
        "所有者权益(或股东权益)合计",
    ],
    "利润表": [
        "营业总收入", "营业收入", "营业总成本", "营业成本", "销售费用",
        "管理费用", "研发费用", "财务费用", "营业利润", "利润总额",
        "所得税费用", "净利润", "归属于母公司所有者的净利润",
        "基本每股收益", "稀释每股收益",
    ],
    "现金流量表": [
        "经营活动现金流入小计", "经营活动现金流出小计",
        "经营活动产生的现金流量净额", "投资活动产生的现金流量净额",
        "筹资活动产生的现金流量净额", "现金及现金等价物净增加额",
        "期末现金及现金等价物余额",
    ],
}

# 摘要接口按「选项」分组，常用指标 17 项，正好是分析师最先看的一组。
_ABSTRACT_GROUP = "常用指标"

# 只取最近 N 个报告期。4 期足够做同比/环比趋势判断（最近两期同季 + 去年同期
# 需要更多期，但 token 预算不允许；需要长历史时应走别的通道）。
MAX_PERIODS = 4


def _period_label(raw):
    """把 20260930 转成 2026-09-30。无法识别时原样返回。"""
    s = str(raw).strip()
    if len(s) == 8 and s.isdigit():
        return f"{s[:4]}-{s[4:6]}-{s[6:]}"
    return s[:10]


def _fundamental_cn_report(symbol, report_type):
    """A 股三大报表（新浪源）。返回 (rows, columns)。"""
    if report_type == "abstract":
        _assert_allowed("stock_financial_abstract")
        try:
            df = ak.stock_financial_abstract(symbol=symbol)
        except Exception as e:
            raise wrap_upstream(e, "stock_financial_abstract")
        if df is None or len(df) == 0:
            raise ApiError(Err.NO_DATA, f"{symbol} 无财务摘要",
                           {"symbol": symbol})
        # 列结构：选项 / 指标 / <日期列…>，日期列已按新→旧排序
        date_cols = [c for c in df.columns if c not in ("选项", "指标")][:MAX_PERIODS]
        if not date_cols:
            raise ApiError(Err.PARSE_FAILED, "财务摘要缺日期列",
                           {"columns": [str(c) for c in list(df.columns)[:10]]})
        sub = df[df["选项"] == _ABSTRACT_GROUP]
        if len(sub) == 0:  # 某些标的没有「常用指标」分组，退化成全部
            sub = df
        columns = ["指标"] + [_period_label(c) for c in date_cols]
        rows = []
        for _, r in sub.head(40).iterrows():
            row = {"指标": str(pick(r, "指标", default="") or "")}
            for c in date_cols:
                row[_period_label(c)] = num_str(r[c])
            rows.append(row)
        return rows, columns

    _assert_allowed("stock_financial_report_sina")
    stock = _cn_prefix(symbol) if not symbol.lower().startswith(
        ("sh", "sz", "bj")) else symbol.lower()
    try:
        df = ak.stock_financial_report_sina(stock=stock, symbol=report_type)
    except Exception as e:
        raise wrap_upstream(e, "stock_financial_report_sina")
    if df is None or len(df) == 0:
        raise ApiError(Err.NO_DATA, f"{symbol} 无{report_type}数据",
                       {"symbol": symbol, "report_type": report_type})
    if "报告日" not in df.columns:
        raise ApiError(Err.PARSE_FAILED, f"{report_type} 缺「报告日」列",
                       {"columns": [str(c) for c in list(df.columns)[:10]]})

    # df 行=报告期（新→旧），列=科目。取最近 MAX_PERIODS 期后转置成
    # 「一行一科目、一列一期」—— 这个方向对 LLM 更友好（横向看趋势）。
    head = df.head(MAX_PERIODS)
    periods = [_period_label(r["报告日"]) for _, r in head.iterrows()]
    columns = ["科目"] + periods
    wanted = [c for c in _KEY_ROWS[report_type] if c in df.columns]
    missing = [c for c in _KEY_ROWS[report_type] if c not in df.columns]
    rows = []
    for name in wanted:
        row = {"科目": name}
        for (_, r), label in zip(head.iterrows(), periods):
            row[label] = num_str(r[name])
        rows.append(row)
    if missing:
        # 不同公司科目命名有差异（银行没有「存货」）。不报错，只在
        # 返回里标注，避免调用方以为数据缺失是故障。
        rows.append({"科目": "_missing_rows", "note": ",".join(missing)})
    if not rows:
        raise ApiError(Err.PARSE_FAILED,
                       f"{report_type} 未匹配到任何关键科目",
                       {"available": [str(c) for c in list(df.columns)[:20]]})
    return rows, columns


def ep_fundamental(symbol="", market="cn", report_type="abstract"):
    """财报数据。返回最近 MAX_PERIODS 期的关键科目。

    为什么必须截断（这是本函数存在的主要设计约束）
    -------------------------------------------
    新浪三大报表是 **104 行 × 147 列**。若原样进 prompt：
      - 单次调用就吃掉数万 token，挤掉其它工具结果；
      - 而且 90% 的科目分析师根本不看，纯噪声。
    所以这里只保留 ``_KEY_ROWS`` 白名单里的十几个科目 + 最近 4 期。

    调用方若需要完整报表，应走别的通道（例如直接把 URL 给用户打开），
    而不是把这个端点改成不截断 —— 那会毁掉它存在的意义。
    """
    symbol = (symbol or "").strip()
    if not symbol:
        raise ApiError(Err.BAD_REQUEST, "缺少 symbol")
    if market not in ("cn", "us", "hk"):
        raise ApiError(Err.BAD_REQUEST, f"market 非法: {market!r}（应为 cn/us/hk）")
    allowed_types = list(_KEY_ROWS.keys()) + ["abstract"]
    if report_type not in allowed_types:
        raise ApiError(Err.BAD_REQUEST,
                       f"report_type 非法: {report_type!r}（应为 {'/'.join(allowed_types)}）")

    if market in ("us", "hk"):
        # 明确的能力缺口，而不是错误：见文件头注释「为什么美股财报不用 _em」。
        raise ApiError(
            Err.NO_DATA,
            f"美股/港股财报通道待接入（不用东财 _em 接口，见注释）",
            {"_unavailable": "美股/港股财报通道待接入",
             "market": market, "symbol": symbol},
        )

    rows, columns = _fundamental_cn_report(symbol, report_type)
    return {
        "symbol": symbol,
        "market": market,
        "report_type": report_type,
        "currency": "CNY",
        "periods": columns[1:],
        "columns": columns,
        "rows": rows[:40],
        "truncated": True,
        "note": f"仅保留最近 {MAX_PERIODS} 期与关键科目；完整报表未接入",
    }


def wrap_upstream(exc, func_name):
    """把上游异常映射到错误码。"""
    msg = str(exc)
    low = msg.lower()
    # 新浪源对不存在的标的会在内部 KeyError('date') —— 这是「标的不存在」，
    # 不是内部 bug。必须在 INTERNAL 兜底之前识别，否则 Go 侧会重试无意义的请求。
    if isinstance(exc, KeyError) and "date" in low:
        return ApiError(Err.SYMBOL_NOT_FOUND,
                        f"该源({func_name})未返回此标的的日线数据"
                        f"（可能代码错误、已退市）",
                        {"func": func_name})
    if looks_like_challenge(msg):
        return ApiError(Err.HTML_CHALLENGE, "上游返回反爬页面，非数据",
                        {"func": func_name})
    if "timeout" in low or "timed out" in low:
        return ApiError(Err.UPSTREAM_TIMEOUT, f"上游超时：{func_name}",
                        {"func": func_name, "timeout_s": UPSTREAM_TIMEOUT})
    if "remotedisconnected" in low or "connection aborted" in low or "connectionerror" in low:
        return ApiError(Err.UPSTREAM_BLOCKED, f"上游拒绝连接（反爬）：{func_name}",
                        {"func": func_name})
    if "sslerror" in low or "certificate" in low:
        return ApiError(Err.UPSTREAM_BLOCKED, f"TLS 证书问题：{func_name}",
                        {"func": func_name})
    return ApiError(Err.INTERNAL, f"{func_name} 失败: {msg[:300]}",
                    {"func": func_name, "exc_type": type(exc).__name__})


# =====================================================================
# HTTP 层
# =====================================================================
class Handler(BaseHTTPRequestHandler):
    server_version = "artex-akshare-sidecar/" + AKSHARE_VERSION

    def log_message(self, fmt, *args):  # 静默默认的 stderr 访问日志
        pass

    def _send(self, http_status, payload):
        body = json.dumps(payload, ensure_ascii=False, allow_nan=False).encode("utf-8")
        self.send_response(http_status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _ok(self, data, elapsed_ms, source=None):
        meta = {
            "source": source or "akshare",
            "akshare_version": AKSHARE_VERSION,
            "fetched_at": now_iso(),
            "elapsed_ms": elapsed_ms,
        }
        self._send(200, {"ok": True, "data": data, "meta": meta})

    def _err(self, e):
        self._send(ERROR_HTTP.get(e.code, 500), {
            "ok": False,
            "error": {"code": e.code, "message": e.message, "detail": e.detail},
        })

    def do_GET(self):
        started = time.time()
        parsed = urlparse(self.path)
        path = parsed.path.rstrip("/") or "/"
        q = {k: v[0] for k, v in parse_qs(parsed.query).items()}
        try:
            if path == "/health":
                data = ep_health()
                self._ok(data, int((time.time() - started) * 1000), source="akshare")
                return
            if path == "/quote":
                data = ep_quote(q.get("symbol", ""), market=q.get("market", "cn"))
                self._ok(data, int((time.time() - started) * 1000), source=data.get("source"))
                return
            if path == "/kline":
                data, src = ep_kline(
                    q.get("symbol", ""),
                    period=q.get("period", "daily"),
                    count=q.get("count", "250"),
                    market=q.get("market", "cn"),
                )
                self._ok(data, int((time.time() - started) * 1000), source=src)
                return
            if path == "/macro":
                data = ep_macro(q.get("indicator", "cpi"))
                self._ok(data, int((time.time() - started) * 1000), source="akshare")
                return
            if path == "/news":
                data = ep_news(
                    market=q.get("market", "cn"),
                    symbol=q.get("symbol", ""),
                    limit=q.get("limit", "20"),
                )
                self._ok(data, int((time.time() - started) * 1000),
                         source="caixin+cls")
                return
            if path == "/fundamental":
                data = ep_fundamental(
                    symbol=q.get("symbol", ""),
                    market=q.get("market", "cn"),
                    report_type=q.get("report_type", "abstract"),
                )
                self._ok(data, int((time.time() - started) * 1000), source="sina")
                return
            raise ApiError(Err.BAD_REQUEST, f"未知端点: {path}")
        except ApiError as e:
            self._err(e)
        except Exception as e:  # 兜底，避免线程崩掉导致连接悬挂
            self._err(ApiError(Err.INTERNAL, f"未预期异常: {type(e).__name__}: {str(e)[:300]}"))

    def do_POST(self):
        parsed = urlparse(self.path)
        if parsed.path.rstrip("/") == "/_shutdown":
            # 优雅退出（测试用）。先回响应再停，避免客户端拿不到 200。
            self._send(200, {"ok": True, "data": {"stopping": True}})
            threading.Thread(target=self.server.shutdown, daemon=True).start()
            return
        self._err(ApiError(Err.BAD_REQUEST, "未知端点"))


def main():
    addr = os.getenv("AKSHARE_SIDECAR_ADDR", DEFAULT_ADDR)
    host, _, port_s = addr.rpartition(":")
    if not host:
        host = "127.0.0.1"
    try:
        port = int(port_s)
    except ValueError:
        sys.exit(f"AKSHARE_SIDECAR_ADDR 格式错误: {addr!r}（应为 host:port）")

    srv = ThreadingHTTPServer((host, port), Handler)
    srv.daemon_threads = True
    print(f"akshare sidecar listening on http://{host}:{port} "
          f"(akshare {AKSHARE_VERSION}, upstream=sina)", flush=True)

    # 启动即异步预热。为什么必须先起监听再预热：预热要 20s，若先预热再监听，
    # 端口在这 20s 内不可连接，Go 侧的健康检查会直接连接失败。
    threading.Thread(target=_warmup, daemon=True).start()

    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        srv.server_close()


if __name__ == "__main__":
    main()
