"""断点状态：按站点保存采集游标，原子写入，支持中断恢复。"""

from __future__ import annotations

import json
import os
import tempfile
from pathlib import Path

from .errors import OpsCollectorError


class CheckpointStore:
    def __init__(self, state_dir: Path):
        self.state_dir = Path(state_dir)
        self.state_dir.mkdir(parents=True, exist_ok=True)

    def _path(self, site_id: str) -> Path:
        return self.state_dir / f"{site_id}.json"

    def load(self, site_id: str) -> dict:
        p = self._path(site_id)
        if not p.exists():
            return {}
        try:
            data = json.loads(p.read_text(encoding="utf-8"))
            return data if isinstance(data, dict) else {}
        except json.JSONDecodeError:
            return {}

    def save(self, site_id: str, state: dict) -> None:
        p = self._path(site_id)
        state = {"schema_version": 1, **state}
        fd, tmp = tempfile.mkstemp(dir=str(self.state_dir), suffix=".tmp")
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as f:
                json.dump(state, f, ensure_ascii=False, indent=2)
            os.replace(tmp, p)
        except Exception as e:
            OpsCollectorError(f"checkpoint 写入失败: {type(e).__name__}")
            try:
                os.unlink(tmp)
            except OSError:
                pass
            raise

    def last_log_timestamp(self, site_id: str) -> int | None:
        return self.load(site_id).get("last_log_timestamp")
