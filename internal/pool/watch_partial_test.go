package pool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestReloadAuthDirPartialWriteDropsAccount 记录一个**已知行为**（不是通过断言，
// 而是通过显式固化）：非原子写入（如 login.sh 的 `open(...,"w")` + json.dump）
// 期间目录里会短暂存在半截 JSON。auth.LoadDir 对坏文件是「跳过」（不报错），
// 于是该 uid 在这一次 reload 里不在 auths 列表中 → SyncToDir 把它从池中剔除。
//
// 这是 fsnotify/轮询式热加载的固有权衡，但对本仓库**曾经真实存在**：fork 的
// login.sh 是非原子写（上游 login.sh 用 tempfile + os.replace 原子替换）。
// 本测试固化「半截文件会导致剔除」这一事实，使任何后续改动（给 login.sh 加原子写、
// 或给 reload 加防抖）都能被察觉。
//
// 结论（已实测）：轮询周期 5s 远大于写入耗时，窗口极窄；且下一次指纹变化
// （写入完成）会立即把账号重新加载回来。故实际影响是「瞬时抖动」而非「永久丢失」。
// 修复选择见 login.sh 的原子写改造（消除该窗口本身，比在 reload 侧加防抖更根本）。
func TestReloadAuthDirPartialWriteDropsAccount(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "u1", "一号")
	writeAuthFile(t, dir, "u2", "二号")

	auths, _ := auth.LoadDir(dir)
	p := New(filepath.Join(t.TempDir(), "state.json"))
	defer p.Close()
	p.SyncToDir(auths)
	if n := len(p.AvailableUIDs()); n != 2 {
		t.Fatalf("前置：账号数=%d want 2", n)
	}

	// 模拟非原子写入的中途状态：u2 的文件被截断成半截 JSON。
	f2 := filepath.Join(dir, "workbuddy-u2.json")
	if err := os.WriteFile(f2, []byte(`{"account":{"uid":"u2"`), 0o600); err != nil {
		t.Fatal(err)
	}
	p.reloadAuthDir(dir)
	if n := len(p.AvailableUIDs()); n != 1 {
		t.Fatalf("半截文件应导致该账号被剔除（LoadDir 跳过坏文件）：账号数=%d want 1", n)
	}

	// 写入完成后（下一个指纹变化）账号被重新加载——证明是瞬时抖动而非永久丢失。
	if err := os.WriteFile(f2, []byte(`{"account":{"uid":"u2","nickname":"二号"},"auth":{"accessToken":"t","refreshToken":"r","expiresAt":9999999999}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p.reloadAuthDir(dir)
	if n := len(p.AvailableUIDs()); n != 2 {
		t.Fatalf("写入完成后应恢复 2 个账号：账号数=%d", n)
	}
}
