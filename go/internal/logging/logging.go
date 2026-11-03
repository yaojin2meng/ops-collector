// Package logging: JSONL 诊断日志。stdout 保持纯净 JSON 输出，
// 诊断一律写 stderr 或 var/logs/*.jsonl。
package logging

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Logger struct {
	mu         sync.Mutex
	file       *os.File
	alsoStderr bool
}

func New(logDir, runID string, alsoStderr bool) *Logger {
	l := &Logger{alsoStderr: alsoStderr}
	if logDir != "" {
		if err := os.MkdirAll(logDir, 0o755); err == nil {
			name := "collector.jsonl"
			if runID != "" {
				name = fmt.Sprintf("run-%s.jsonl", runID)
			}
			if f, err := os.OpenFile(filepath.Join(logDir, name),
				os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
				l.file = f
			}
		}
	}
	return l
}

func (l *Logger) log(level, event string, fields map[string]any) {
	rec := map[string]any{
		"timestamp": time.Now().Unix(),
		"level":     level,
		"event":     event,
	}
	for k, v := range fields {
		rec[k] = v
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		l.file.Write(append(b, '\n'))
	}
	if l.alsoStderr {
		os.Stderr.Write(append(b, '\n'))
	}
}

func (l *Logger) Info(event string, fields map[string]any)  { l.log("info", event, fields) }
func (l *Logger) Warn(event string, fields map[string]any)  { l.log("warn", event, fields) }
func (l *Logger) Error(event string, fields map[string]any) { l.log("error", event, fields) }

func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
}
