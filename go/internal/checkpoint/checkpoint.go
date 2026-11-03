// Package checkpoint: 断点状态。按站点保存采集游标，原子写入，支持中断恢复。
package checkpoint

import (
	"encoding/json"
	"os"
	"path/filepath"

	ierr "opscollector/internal/errors"
)

type Store struct {
	dir string
}

func NewStore(dir string) *Store {
	_ = os.MkdirAll(dir, 0o755)
	return &Store{dir: dir}
}

func (s *Store) path(siteID string) string {
	return filepath.Join(s.dir, siteID+".json")
}

func (s *Store) Load(siteID string) map[string]any {
	b, err := os.ReadFile(s.path(siteID))
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil || m == nil {
		return map[string]any{}
	}
	return m
}

func (s *Store) Save(siteID string, state map[string]any) error {
	full := map[string]any{"schema_version": 1}
	for k, v := range state {
		full[k] = v
	}
	b, err := json.MarshalIndent(full, "", "  ")
	if err != nil {
		return ierr.Wrap(ierr.CodeInternal, "checkpoint 序列化失败", err)
	}
	tmp, err := os.CreateTemp(s.dir, siteID+"-*.tmp")
	if err != nil {
		return ierr.Wrap(ierr.CodeInternal, "checkpoint 临时文件创建失败", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return ierr.Wrap(ierr.CodeInternal, "checkpoint 写入失败", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return ierr.Wrap(ierr.CodeInternal, "checkpoint 关闭失败", err)
	}
	if err := os.Rename(name, s.path(siteID)); err != nil {
		os.Remove(name)
		return ierr.Wrap(ierr.CodeInternal, "checkpoint 原子替换失败", err)
	}
	return nil
}

func (s *Store) LastLogTimestamp(siteID string) (int64, bool) {
	m := s.Load(siteID)
	if v, ok := m["last_log_timestamp"].(float64); ok {
		return int64(v), true
	}
	return 0, false
}
