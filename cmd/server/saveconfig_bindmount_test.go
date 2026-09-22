package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// withEBUSYRename 把 configRename 替换为「总是返回 EBUSY」并返回恢复函数。
//
// 为什么需要注入：Docker 单文件 bind mount 上 os.Rename 覆盖挂载目标会返回 EBUSY，
// saveConfig 对此有「就地改写挂载文件」的回退路径。Windows 允许 rename 覆盖已存在
// 文件，该路径永远不会自然触发，只能靠注入验证（否则是一段无人走过的死代码）。
func withEBUSYRename(t *testing.T) {
	t.Helper()
	orig := configRename
	configRename = func(_, _ string) error { return syscall.EBUSY }
	t.Cleanup(func() { configRename = orig })
}

// withFailingRename 把 configRename 替换为「返回非 EBUSY 错误」：应保持原样报错，
// 绝不走 bind mount 回退（回退会 O_TRUNC 破坏目标文件，不能对普通错误误用）。
func withFailingRename(t *testing.T, err error) {
	t.Helper()
	orig := configRename
	configRename = func(_, _ string) error { return err }
	t.Cleanup(func() { configRename = orig })
}

// newSaveConfigFixture 准备一份最小可加载的 config.json 与 saveConfig 依赖。
func newSaveConfigFixture(t *testing.T) (path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":":7863","api_key":"orig-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	live = &livecfg.Holder{}
	p = pool.New(filepath.Join(dir, "state.json"))
	t.Cleanup(func() { p.Close() })
	up = upstream.New()
	sch = scheduler.New(scheduler.Config{})
	return path, live, p, up, sch
}

// TestSaveConfigBindMountFallbackWritesInPlace EBUSY（Docker 单文件 bind mount）时
// 必须就地改写挂载文件：内容为新配置、无 .tmp 残留、不返回错误。
func TestSaveConfigBindMountFallbackWritesInPlace(t *testing.T) {
	path, live, p, up, sch := newSaveConfigFixture(t)
	withEBUSYRename(t)

	if _, err := saveConfig([]byte(`{"api_key":"new-key"}`), path, live, p, up, sch); err != nil {
		t.Fatalf("EBUSY fallback must succeed, got: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "new-key") {
		t.Errorf("bind mount fallback must write new content in place, got: %s", got)
	}
	if strings.Contains(string(got), "orig-key") {
		t.Errorf("stale key survived the in-place rewrite: %s", got)
	}
	// 写成功后必须清理 tmp（否则每次保存都在配置目录里留垃圾）。
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tmp file must be removed after a successful fallback write, stat err=%v", err)
	}
}

// TestSaveConfigNonEBUSYErrorDoesNotTruncate 非 EBUSY 的 rename 失败必须原样报错，
// **不得**走 bind mount 回退——回退会 O_TRUNC 破坏目标文件，对普通错误误用会丢配置。
func TestSaveConfigNonEBUSYErrorDoesNotTruncate(t *testing.T) {
	path, live, p, up, sch := newSaveConfigFixture(t)
	withFailingRename(t, errors.New("permission denied"))

	if _, err := saveConfig([]byte(`{"api_key":"new-key"}`), path, live, p, up, sch); err == nil {
		t.Fatal("non-EBUSY rename failure must return an error")
	}

	// 原文件必须完好（未被 O_TRUNC 破坏）。
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "orig-key") {
		t.Errorf("non-EBUSY failure must not modify the config file, got: %s", got)
	}
}

// TestSaveConfigNormalPathStillAtomic 常规路径（rename 成功）不受回退影响：
// 内容更新、无 tmp 残留。
func TestSaveConfigNormalPathStillAtomic(t *testing.T) {
	path, live, p, up, sch := newSaveConfigFixture(t)

	if _, err := saveConfig([]byte(`{"api_key":"normal-key"}`), path, live, p, up, sch); err != nil {
		t.Fatalf("normal save must succeed, got: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "normal-key") {
		t.Errorf("normal path must write new content, got: %s", got)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tmp file must be removed on the normal path, stat err=%v", err)
	}
}
