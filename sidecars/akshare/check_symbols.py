"""_symbol_needles 的自检（改 _SYMBOL_ALIASES 后跑一次）。

    python3 sidecars/akshare/check_symbols.py

不依赖真实网络：stub 掉 akshare/pandas 只加载模块里的纯函数。
守的是「按标的过滤新闻」这条路的正确性 —— 它错了会静默返回空列表，
而上层会把空读成「这只票没有新闻」（2026-11-09 实测 NVDA 就是这样归零的）。
"""

import importlib.util
import os
import sys
import types

# stub 掉第三方依赖：server.py 顶层 import akshare / pandas 只为取版本号。
sys.modules.setdefault("akshare", types.ModuleType("akshare"))
_pd = types.ModuleType("pandas")
_pd.__version__ = "3.1.1"
sys.modules.setdefault("pandas", _pd)

_here = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location("sidecar", os.path.join(_here, "server.py"))
m = importlib.util.module_from_spec(_spec)
try:
    _spec.loader.exec_module(m)
except SystemExit:
    pass  # server.py 在缺依赖时 sys.exit，这里已 stub 过，不会走到

ns = m._symbol_needles

# 美股：代码本体与中文别名都要命中（中文源不会出现 "NVDA" 这个字符串）
assert "nvda" in ns("NVDA"), ns("NVDA")
assert "英伟达" in ns("NVDA"), ns("NVDA")
assert "tsla" in ns("TSLA") and "特斯拉" in ns("TSLA"), ns("TSLA")
assert "aapl" in ns("AAPL") and "苹果" in ns("AAPL"), ns("AAPL")

# 大小写不敏感
assert ns("nvda") == ns("NVDA")

# A 股/港股：纯数字代码
assert "600519" in ns("sh600519"), ns("sh600519")
assert "00700" in ns("00700")

# 指数占位符 → 不做标的过滤（回空列表，调用方据此跳过筛选）
assert ns("^VIX") == [], ns("^VIX")

# 空输入
assert ns("") == []

# 未收录的代码：只回代码本身，不瞎猜别名
assert ns("ZZZZ") == ["zzzz"], ns("ZZZZ")

print("check_symbols: all assertions passed")
