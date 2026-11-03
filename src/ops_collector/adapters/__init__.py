"""adapter 包：按站点类型路由到具体实现。"""

from __future__ import annotations

from .base import SiteAdapter, make_adapter

__all__ = ["SiteAdapter", "make_adapter"]
