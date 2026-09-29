// Package livecfg 运行期可变配置的并发安全持有者。
//
// 背景：进程启动时读入的配置是普通字段（读多写零），但管理面板允许在线改配置，
// 于是少量"可热生效"的字段需要有并发安全的读写点。此处用不可变快照 + atomic 指针：
// 读方 Load 拿到一致视图，写方 Store 整体替换，无锁无数据竞争。
//
// 只承载**读路径深、热改需求强**的少数字段；池参数/排程参数等各有既有 setter
// （pool.SetBreaker、scheduler.Reconfigure 等），不重复收编到这里。
package livecfg

import (
	"sync/atomic"
	"time"
)

// Snapshot 一次读取的不可变配置视图。
type Snapshot struct {
	// APIKey 数据面（/v1/*）鉴权密钥；空 = 不鉴权。
	//
	// 注意：本键是**必须外发**的凭证（opencode / Claude Code / 各类 SDK 都要填），
	// 故它不应同时充当面板的管理凭证——面板默认复用本值是历史行为，设置 PanelKey
	// 后即解耦（见下）。
	APIKey string
	// PanelKey 管理面（/panel/*）独立鉴权密钥。
	//
	// 非空：面板**只**认本值——持有数据面 api_key 的客户端/日志/截图不再等于拿到
	// 账号池管理权（面板能读到全部账号 uid/昵称/余额，并能改动账号池与配置）。
	// 空：回落复用 APIKey（向后兼容；升级后不设也不会打不开面板）。
	PanelKey             string
	SoftCooldown         time.Duration // 429 软冷却基数（<=0 时回落服务启动值或默认）
	SanitizeFingerprints bool          // 出站请求体指纹脱敏
	RealmFallback        bool          // 跨域回落（cn/global 账号互备；config global.realm_fallback）
}

// Holder 原子持有当前快照。
type Holder struct {
	p atomic.Pointer[Snapshot]
}

// New 以初始快照构建。
func New(s Snapshot) *Holder {
	h := &Holder{}
	h.Store(s)
	return h
}

// Load 返回当前快照（Holder 为 nil 或从未 Store 时返回零值快照，调用方无需判空）。
func (h *Holder) Load() Snapshot {
	if h == nil {
		return Snapshot{}
	}
	if s := h.p.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}

// Store 整体替换快照。
func (h *Holder) Store(s Snapshot) { h.p.Store(&s) }
