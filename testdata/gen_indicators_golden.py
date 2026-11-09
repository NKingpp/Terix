#!/usr/bin/env python3
"""生成 agent/indicators.go 的黄金值（golden values）。

运行方式（需先装 TA-Lib）::

    python3 -m venv .venv-talib
    .venv-talib/bin/pip install TA-Lib
    .venv-talib/bin/python testdata/gen_indicators_golden.py

输入：``testdata/qqq_2020_2021.psv``（QQQ 2020-01-02..2021-12-31 真实日线，
505 根，由 ``SELECT ... FROM bars`` 导出，见下方 INPUT_SQL）。

输出：``testdata/indicators_golden.json``

为什么用真实数据而不是合成序列
------------------------------
1. 覆盖 2020-03 熔断的极端行情 —— 极端行情最容易暴露实现错误（除零、
   分母接近 0、单边行情下 RSI 的 100 边界等）。
2. 测试输入不硬编码：如果 bars 导入逻辑变化导致数据变，黄金值对不上时能
   通过 ``_meta.bars_sha256`` 立刻分辨「是数据变了」还是「算法变了」。

TA-Lib 参数约定（**这些是 TA-Lib 的源码行为，不是文档抄来的**）
--------------------------------------------------------------
1. **EMA 初始化用 SMA 种子**：TA-Lib 的 EMA 不是「第一个值直接当种子」，
   而是「前 period 个值的简单平均」作为第一个 EMA，之后递推
   ``EMA_t = EMA_{t-1} + α(x_t - EMA_{t-1})``，``α = 2/(period+1)``。
   用错种子会让整条序列偏移，且前几十个值与 TA-Lib 差很多。

2. **RSI / ATR 用 Wilder 平滑**，``α = 1/period``，**不是** ``2/(period+1)``。
   初值是前 period 个涨跌幅（或 TR）的简单平均。这是最常见的实现错误 ——
   很多人拿 EMA 的 α 套在 RSI 上，结果数值全错。

3. **BOLL 用总体标准差**（``ddof=0``）而不是样本标准差（``ddof=1``）。
   numpy 的 ``np.std()`` 默认 ddof=0，pandas 的 ``.std()`` 默认 ddof=1；
   用错差一个系数 ``sqrt(n/(n-1))``。

4. **VWAP 是累积型**（从序列起点累计 ``Σ(price×vol)/Σvol``），不是滚动窗口。
   按交易日重置的 VWAP 由调用方分组后分别调用，指标函数本身不知道「交易日」。

KDJ 参数
--------
国内 A 股惯例是 (9,3,3)，TA-Lib 默认是 (5,3,3)。**两个都生成**：
- ``KDJ_CN_933``：主测试用，国内惯例
- ``KDJ_TALibDefault_533``：辅助测试用，证明实现框架与 TA-Lib 默认值也对得上
  如果只测 (9,3,3)，无法区分「实现正确」与「参数凑巧」。
"""

import hashlib
import json
import os
import sys
from datetime import datetime, timezone

import numpy as np

try:
    import talib
except ImportError:
    sys.exit(
        "需要 TA-Lib。安装：\n"
        "  python3 -m venv .venv-talib && .venv-talib/bin/pip install TA-Lib\n"
        "  然后用 .venv-talib/bin/python 运行本脚本"
    )

HERE = os.path.dirname(os.path.abspath(__file__))
INPUT = os.path.join(HERE, "qqq_2020_2021.psv")
OUTPUT = os.path.join(HERE, "indicators_golden.json")

# 输入数据的来源 SQL，写进 meta 便于复现
INPUT_SQL = (
    "select b.trading_date||'|'||b.open||'|'||b.high||'|'||b.low||'|'||b.close||'|'||b.volume "
    "from bars b join instruments i on i.id=b.instrument_id "
    "where i.symbol='QQQ' and trading_date between '2020-01-02' and '2021-12-31' "
    "and not b.is_partial order by b.trading_date"
)

# 抽样点：首值、末值、以及中间均匀分布的若干点。
# 不做全量对撞（505 点 × 6 指标会让 golden 文件巨大且无额外价值），
# 但要覆盖 warmup 之后的前几个值（最易错）与最后的稳态值。
SAMPLE_INDICES = [0, 1, 2, 5, 10, 13, 14, 15, 19, 20, 33, 34, 40, 60, 100, 150, 200, 250, 300, 350, 400, 450, 500, 503, 504]


def load_psv(path):
    """读 PSV，返回 (dates, o, h, l, c, v) 全部为 numpy 数组。"""
    dates, o, h, l, c, v = [], [], [], [], [], []
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            parts = line.split("|")
            if len(parts) != 6:
                raise ValueError(f"格式错误（需要 6 段）：{line!r}")
            dates.append(parts[0])
            o.append(float(parts[1]))
            h.append(float(parts[2]))
            l.append(float(parts[3]))
            c.append(float(parts[4]))
            v.append(float(parts[5]))
    return dates, np.array(o), np.array(h), np.array(l), np.array(c), np.array(v)


def to_samples(arr):
    """把数组转成 {index: value} 的抽样字典。

    NaN 一律写成 null（JSON 没有 NaN）。Go 侧断言 warmup 段是空字符串。
    用 repr 保留足够精度（float64 的 17 位有效数字），避免生成时就丢精度。
    """
    out = {}
    for i in SAMPLE_INDICES:
        if i >= len(arr):
            continue
        val = float(arr[i])
        out[str(i)] = None if np.isnan(val) else repr(val)
    return out


def first_valid(arr):
    """返回第一个非 NaN 的下标；全 NaN 返回 -1。"""
    idx = np.where(~np.isnan(arr))[0]
    return int(idx[0]) if len(idx) else -1


def main():
    if not os.path.exists(INPUT):
        sys.exit(f"找不到输入文件 {INPUT}，先用 psql 导出（SQL 见本文件 INPUT_SQL）")

    dates, o, h, l, c, v = load_psv(INPUT)
    if len(c) < 40:
        sys.exit(f"输入只有 {len(c)} 根 bar，不足以覆盖 MACD 的 34 根 warmup")

    golden = {}

    # ---- MACD(12,26,9) ----
    macd, macdsig, macdhist = talib.MACD(c, fastperiod=12, slowperiod=26, signalperiod=9)
    golden["MACD"] = {
        "params": {"fast": 12, "slow": 26, "signal": 9},
        "first_valid": {"macd": first_valid(macd), "signal": first_valid(macdsig), "hist": first_valid(macdhist)},
        "samples": {
            "macd": to_samples(macd),
            "signal": to_samples(macdsig),
            "hist": to_samples(macdhist),
        },
    }

    # ---- RSI(14)，Wilder 平滑 ----
    rsi = talib.RSI(c, timeperiod=14)
    golden["RSI"] = {
        "params": {"period": 14},
        "first_valid": first_valid(rsi),
        "samples": to_samples(rsi),
    }

    # ---- ATR(14)，Wilder 平滑 ----
    atr = talib.ATR(h, l, c, timeperiod=14)
    golden["ATR"] = {
        "params": {"period": 14},
        "first_valid": first_valid(atr),
        "samples": to_samples(atr),
    }

    # ---- BOLL(20, 2σ)，matype=0 是 SMA，标准差 ddof=0 ----
    bb_up, bb_mid, bb_low = talib.BBANDS(c, timeperiod=20, nbdevup=2.0, nbdevdn=2.0, matype=0)
    golden["BOLL"] = {
        "params": {"period": 20, "k": 2.0, "matype": 0},
        "first_valid": {"upper": first_valid(bb_up), "mid": first_valid(bb_mid), "lower": first_valid(bb_low)},
        "samples": {
            "upper": to_samples(bb_up),
            "mid": to_samples(bb_mid),
            "lower": to_samples(bb_low),
        },
    }

    # ---- KDJ 国内惯例 (9,3,3)：主测试 ----
    k933, d933 = talib.STOCH(h, l, c, fastk_period=9, slowk_period=3, slowk_matype=0,
                             slowd_period=3, slowd_matype=0)
    # J = 3K - 2D 是国内 KDJ 的定义（TA-Lib 不输出 J，自己算）
    j933 = 3 * k933 - 2 * d933
    golden["KDJ_CN_933"] = {
        "params": {"k_period": 9, "d_period": 3, "smooth": 3},
        "first_valid": {"k": first_valid(k933), "d": first_valid(d933), "j": first_valid(j933)},
        "samples": {"k": to_samples(k933), "d": to_samples(d933), "j": to_samples(j933)},
    }

    # ---- KDJ TA-Lib 默认 (5,3,3)：辅助测试，证明框架正确 ----
    k533, d533 = talib.STOCH(h, l, c)
    j533 = 3 * k533 - 2 * d533
    golden["KDJ_TALibDefault_533"] = {
        "params": {"k_period": 5, "d_period": 3, "smooth": 3},
        "first_valid": {"k": first_valid(k533), "d": first_valid(d533), "j": first_valid(j533)},
        "samples": {"k": to_samples(k533), "d": to_samples(d533), "j": to_samples(j533)},
    }

    # ---- VWAP 累积型 ----
    vwap = talib.VWAP(h, l, c, v)
    golden["VWAP"] = {
        "params": {"mode": "cumulative"},
        "first_valid": first_valid(vwap),
        "samples": to_samples(vwap),
    }

    # ---- meta：版本 + 输入指纹 ----
    with open(INPUT, "rb") as f:
        bars_sha = hashlib.sha256(f.read()).hexdigest()

    out = {
        "_meta": {
            "talib_version": talib.__version__,
            "numpy_version": np.__version__,
            "symbol": "QQQ",
            "range": f"{dates[0]}..{dates[-1]}",
            "bar_count": len(c),
            "bars_sha256": bars_sha,
            "input_file": os.path.basename(INPUT),
            "input_sql": INPUT_SQL,
            "sample_indices": SAMPLE_INDICES,
            "generated_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
            "notes": [
                "EMA 用 SMA 种子，α=2/(period+1)",
                "RSI/ATR 用 Wilder 平滑，α=1/period（不是 2/(period+1)）",
                "BOLL 用总体标准差 ddof=0（不是样本标准差）",
                "VWAP 是累积型，按日重置由调用方分组实现",
                "J = 3K - 2D（TA-Lib 不输出 J）",
            ],
        },
    }
    out.update(golden)

    with open(OUTPUT, "w", encoding="utf-8") as f:
        json.dump(out, f, ensure_ascii=False, indent=2)
        f.write("\n")

    print(f"写入 {OUTPUT}")
    print(f"  bar_count = {len(c)}")
    print(f"  bars_sha256 = {bars_sha[:16]}...")
    for key in ("MACD", "RSI", "ATR", "BOLL", "KDJ_CN_933", "KDJ_TALibDefault_533", "VWAP"):
        fv = golden[key]["first_valid"]
        print(f"  {key:22} first_valid = {fv}")


if __name__ == "__main__":
    main()
