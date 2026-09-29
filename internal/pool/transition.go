// 账号状态机迁移的唯一权威实现。
//
// entry 的「可选择性」由四个正交维度决定：禁用(disabled)、账号级冷却(until/coolKind)、
// 模型级冷却(modelCooldowns)、熔断(breakerUntil)。维度之间以「迁移原语」收拢，
// 禁止在其他文件散写这些字段——所有入口（applyErrorPolicy / refresh / keepalive /
// 签到 / 选号）对状态的改动都必须经本文件的原语或经 Cooldown/NoteError/NoteSuccess 等
// 封装（它们在持锁下调用本文件原语）。
//
// 迁移矩阵（事件 → 动作 → 字段）：
//
//	disabled           ← disableLocked（Disable / NoteSessionDead 达阈）
//	until/coolKind     ← Cooldown(CoolSoft/Hard) / CooldownSoftForModel 无解析分支
//	modelCooldowns     ← CooldownSoftForModel 有解析分支；被 disableLocked/Cooldown/clearCoolingLocked 清
//	                     （reviveCoolingLocked 不清——余额恢复不构成限流解除证据）
//	breakerUntil       ← recordBreakerFailureLocked（Cooldown/NoteError 喂入）；NoteSuccess 清
//	softStreak         ← Cooldown(CoolSoft)/CooldownSoftForModel；NoteSuccess 清（revive 保留：与余额无关）
//	sessionDeadFails   ← NoteSessionDead；ClearSessionDead/NoteSuccess/ReviveDisabled 清
//
// 关键正交性（疑点 4 修正）：
//   - 冷却域（until/coolKind/softStreak/modelCooldowns）与熔断器（fails/retryCount/
//     breakerUntil）正交：冷却管「近期被限流/余额耗尽」，熔断管「反复 5xx 失败」。
//     disableLocked 只清冷却域、不动熔断——禁用是授权/session 终态，不应覆盖熔断观测。
//   - clearCoolingLocked 是「冷却域归零」的单一来源，被 disableLocked 共用（禁用是
//     终态，冷却随之作废）。reviveCoolingLocked（签到/余额刷新解冻）**不再**走全清：
//     余额恢复只解冻余额耗尽冷却（CoolHard），软限流退避与模型级台账各有自身恢复
//     时刻（上游重置墙钟到期/探测成功），详见 reviveCoolingLocked 注释。
package pool

import "time"

// clearCoolingLocked 清冷却域：until/coolKind/softStreak/modelCooldowns 全归零，
// reason 一并清空。熔断器（fails/retryCount/breakerUntil）不属冷却域，不动。
// 唯一调用方是 disableLocked（禁用是终态，冷却随之作废）——reviveCoolingLocked
// 自 upstream 602ed1b 起不再走全清（余额恢复只解冻 CoolHard），见其注释。
// 调用方必须已持有 p.mu。
func (e *entry) clearCoolingLocked() {
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
	e.modelCooldowns = nil // 冷却域清零时一并清模型级独立冷却（模型豁免随之消失）
}

// disableLocked 禁用迁移：置 disabled 并清冷却域（禁用是比冷却更强的不可用终态）。
//
// 旧 Disable 只置 disabled+reason，不碰 until/modelCooldowns/softStreak，会出现
// 「disabled=true 但 cooling=true / 残留 modelCooldowns」的一致性问题——一个先被
// 硬冷却（到次日 04:00）再被禁用的账号会同时呈现两种状态。禁用后冷却无意义
// （账号已退出选号，冷却截止不再被读取），故一并清空。
//
// 熔断器保留：熔断是「连续 5xx 失败」信号（与授权/会话无关），禁用后再复活时
// 熔断观测仍有效，不应被禁用覆盖。
func (p *Pool) disableLocked(e *entry, reason string) {
	e.clearCoolingLocked()
	e.disabled = true
	e.reason = reason
	p.dirty.Store(true)
}

// reviveCoolingLocked 余额恢复解冻（签到 / 余额刷新）：更新 credits/creditsTotal，
// 并且**只解冻余额耗尽冷却**——CoolHard 的 until/coolKind/reason。
//
// 为何不再走 clearCoolingLocked（upstream 602ed1b）：余额刷新周期任务（每 5 分钟）
// 经 ReenableIfCredits 到达这里。旧实现无条件清空整个冷却域，于是
//   - 6004 模型级台账（modelCooldowns，对齐上游重置墙钟、可长达数小时）被抹；
//   - CoolSoft 软限流退避与 softStreak 退避计数被清零；
//
// 撞限号被误判健康 → 重新选中 → 再撞 429，全池冷却保护的实际寿命被压到一个刷新
// 周期（≤5min）内（两号池实测复现：expiring==0 的号每 5 分钟被抹一次台账，
// expiring>0 的号走 SetCreditsDetailed 幸免，两号行为不对称即根因指纹）。
// 限流冷却的恢复证据是上游重置墙钟到期或探测成功，不是「余额有钱」；
// softStreak 与余额无关，由 NoteSuccess（成功是最强恢复证据）或自然到期收敛。
// 硬冷却（CoolHard）的权威恢复证据正是余额恢复（remain>0），照旧解冻。
//
// 熔断器（fails/retryCount/breakerUntil）一律不动：签到成功只证明余额与 billing
// 通道健康，不证明 chat 通道健康。调用方必须已持有 p.mu。
func (p *Pool) reviveCoolingLocked(e *entry, credits, total int64) {
	e.credits = credits
	e.creditsTotal = total
	if e.coolKind == CoolHard {
		e.until = time.Time{}
		e.coolKind = 0
		e.reason = ""
	}
}
