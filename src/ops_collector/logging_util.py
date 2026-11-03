"""JSONL 诊断日志：stdout 保持纯净 JSON 输出，诊断一律写 stderr 或文件。"""

from __future__ import annotations

import json
import sys
import threading
import time
from pathlib import Path


class JsonlLogger:
    def __init__(self, log_dir: Path | None = None, *, run_id: str | None = None,
                 also_stderr: bool = True):
        self._lock = threading.Lock()
        self._fh = None
        self._also_stderr = also_stderr
        if log_dir is not None:
            log_dir.mkdir(parents=True, exist_ok=True)
            name = f"run-{run_id}.jsonl" if run_id else "collector.jsonl"
            self._fh = open(log_dir / name, "a", encoding="utf-8")

    def log(self, event: str, *, level: str = "info", **fields) -> None:
        rec = {"timestamp": int(time.time()), "level": level,
               "event": event, **fields}
        line = json.dumps(rec, ensure_ascii=False, default=str)
        with self._lock:
            if self._fh:
                self._fh.write(line + "\n")
                self._fh.flush()
            if self._also_stderr:
                print(line, file=sys.stderr)

    def info(self, event: str, **fields) -> None:
        self.log(event, level="info", **fields)

    def warn(self, event: str, **fields) -> None:
        self.log(event, level="warn", **fields)

    def error(self, event: str, **fields) -> None:
        self.log(event, level="error", **fields)

    def close(self) -> None:
        if self._fh:
            self._fh.close()
            self._fh = None
