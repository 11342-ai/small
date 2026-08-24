package trace

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendWritesJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.trace.jsonl")
	s := New(path)
	e := Entry{
		TS:         time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC),
		Session:    "mychat",
		Round:      1,
		Name:       "echo",
		Args:       `{"v":"hello"}`,
		Data:       "hello",
		IsError:    false,
		DurationMs: 12,
	}
	if err := s.Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	line := strings.TrimSpace(string(data))
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("unmarshal line %q: %v", line, err)
	}
	// 字段名对齐设计（snake_case 短名，人读可查），时间戳 RFC3339。
	if got["ts"] != "2026-08-24T10:00:00Z" {
		t.Errorf("ts = %v", got["ts"])
	}
	for _, k := range []string{"session", "round", "name", "args", "data", "isError", "durationMs"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing field %q in line %q", k, line)
		}
	}
}

func TestAppendAppendsNotTruncates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.trace.jsonl")
	s := New(path)
	for i := 0; i < 3; i++ {
		e := Entry{TS: time.Now(), Session: "s", Round: i, Name: "echo"}
		if err := s.Append(e); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		n++
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n != 3 {
		t.Errorf("lines = %d, want 3（追加不得截断既有内容）", n)
	}
}

func TestNoAppendNoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.trace.jsonl")
	New(path) // 不 Append：零副作用，不应产生文件
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file exists before any Append, want absent（惰性打开）")
	}
}

func TestAppendOpenError(t *testing.T) {
	// 目标目录不存在（也未创建过）：首次 Append 打开失败应返回 error，而非 panic/静默。
	s := New(filepath.Join(t.TempDir(), "no-such-dir", "s.trace.jsonl"))
	if err := s.Append(Entry{TS: time.Now(), Name: "echo"}); err == nil {
		t.Fatal("Append to nonexistent dir: want error, got nil")
	}
}
