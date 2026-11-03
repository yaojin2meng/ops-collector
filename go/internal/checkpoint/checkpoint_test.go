package checkpoint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRoundtrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s := NewStore(dir)
	if _, ok := s.LastLogTimestamp("s1"); ok {
		t.Fatal("新 store 不应有断点")
	}
	if err := s.Save("s1", map[string]any{"last_log_timestamp": int64(12345)}); err != nil {
		t.Fatal(err)
	}
	v, ok := s.LastLogTimestamp("s1")
	if !ok || v != 12345 {
		t.Fatalf("last_log_timestamp = %d", v)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "s1.json"))
	var m map[string]any
	if json.Unmarshal(b, &m) != nil || m["schema_version"].(float64) != 1 {
		t.Fatal("schema_version 缺失")
	}
}

func TestCorruptFileTolerated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s1.json"),
		[]byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewStore(dir)
	if m := s.Load("s1"); len(m) != 0 {
		t.Fatal("损坏文件应返回空 map")
	}
}

func TestChineseNames(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "状态")
	s := NewStore(dir)
	if err := s.Save("站点甲", map[string]any{"last_log_timestamp": int64(9)}); err != nil {
		t.Fatal(err)
	}
	if v, ok := NewStore(dir).LastLogTimestamp("站点甲"); !ok || v != 9 {
		t.Fatal("中文路径/站点名存取失败")
	}
}
