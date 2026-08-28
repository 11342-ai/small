package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProposedStore_AddPendingClear(t *testing.T) {
	s := NewProposedStore()
	if id := s.Add(ProposedEdit{Path: "a.txt", Kind: "write", Content: "x"}); id != 1 {
		t.Fatalf("首条 id = %d, want 1", id)
	}
	s.Add(ProposedEdit{Path: "b.txt", Kind: "edit", Content: "y"})
	if got := len(s.Pending()); got != 2 {
		t.Fatalf("Pending = %d, want 2", got)
	}
	s.Clear()
	if got := len(s.Pending()); got != 0 {
		t.Fatalf("Clear 后 Pending = %d, want 0", got)
	}
}

func TestRunProposeFileEdit_StagedNotWritten(t *testing.T) {
	root := newFileWorkspace(t)
	store := NewProposedStore()
	ctx := WithProposals(context.Background(), store)
	cfg := &FileConfig{Root: root}

	res, err := runProposeFileEdit(ctx, cfg, editArgs("README.md", false,
		editReplacement{Old: "JSONL", New: "JSON Lines"}))
	if err != nil || res.IsError {
		t.Fatalf("提议失败：res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Data, "已提议 #1") {
		t.Fatalf("应含提议编号，got %q", res.Data)
	}
	// 不碰盘：文件内容保持原样。
	if got := readWorkspace(t, root, "README.md"); !strings.Contains(got, "JSONL") || strings.Contains(got, "JSON Lines") {
		t.Fatalf("提议不应落盘，got %q", got)
	}
	// store 记录落地内容（替换后的完整内容）。
	pend := store.Pending()
	if len(pend) != 1 || pend[0].Kind != "edit" || !strings.Contains(pend[0].Content, "JSON Lines") {
		t.Fatalf("store 应含替换后内容，got %+v", pend)
	}
}

func TestRunProposeFileWrite_StagedNotWritten(t *testing.T) {
	root := newFileWorkspace(t)
	store := NewProposedStore()
	ctx := WithProposals(context.Background(), store)
	cfg := &FileConfig{Root: root}

	res, err := runProposeFileWrite(ctx, cfg, writeArgs("internal/agent/new.md", "hello\n"))
	if err != nil || res.IsError {
		t.Fatalf("提议失败：res=%+v err=%v", res, err)
	}
	// 不碰盘：文件不存在。
	if _, err := os.Stat(filepath.Join(root, "internal/agent/new.md")); !os.IsNotExist(err) {
		t.Fatalf("提议不应创建文件，err=%v", err)
	}
}

func TestRunPropose_NoStoreFailClosed(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}
	// 无 store（ctx 未注入）→ 拒绝提议。
	res, err := runProposeFileEdit(context.Background(), cfg, editArgs("README.md", false,
		editReplacement{Old: "JSONL", New: "X"}))
	if err != nil || !res.IsError || !strings.Contains(res.Data, "不可用") {
		t.Fatalf("无 store 应拒绝，got res=%+v err=%v", res, err)
	}
	res, err = runProposeFileWrite(context.Background(), cfg, writeArgs("a.txt", "x"))
	if err != nil || !res.IsError || !strings.Contains(res.Data, "不可用") {
		t.Fatalf("无 store 应拒绝，got res=%+v err=%v", res, err)
	}
}

func TestRunProposeFileEdit_InvalidRejected(t *testing.T) {
	root := newFileWorkspace(t)
	store := NewProposedStore()
	ctx := WithProposals(context.Background(), store)
	cfg := &FileConfig{Root: root}

	// old_string 未找到：提议当场拒绝，不让用户确认注定失败的改动。
	res, err := runProposeFileEdit(ctx, cfg, editArgs("README.md", false,
		editReplacement{Old: "不存在的词", New: "x"}))
	if err != nil || !res.IsError || !strings.Contains(res.Data, "提议无效") {
		t.Fatalf("无效提议应拒绝，got res=%+v err=%v", res, err)
	}
	if len(store.Pending()) != 0 {
		t.Fatalf("无效提议不应入 store，got %+v", store.Pending())
	}
}

func TestApplyProposed(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}

	if err := ApplyProposed(cfg, ProposedEdit{Path: "internal/agent/new.md", Kind: "write", Content: "hello\n"}); err != nil {
		t.Fatalf("落地失败: %v", err)
	}
	if got := readWorkspace(t, root, "internal/agent/new.md"); got != "hello\n" {
		t.Fatalf("落地内容不符，got %q", got)
	}
	// 越界路径落地拒绝。
	if err := ApplyProposed(cfg, ProposedEdit{Path: "../evil.txt", Kind: "write", Content: "x"}); err == nil || !strings.Contains(err.Error(), "越界") {
		t.Fatalf("越界落地应拒绝，got %v", err)
	}
}
