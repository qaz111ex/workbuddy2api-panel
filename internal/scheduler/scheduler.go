// Package scheduler 定时任务：签到 / 活跃上报 / 猫猫旅行 / token keepalive /
// 夜猫子 / 成长任务队列 六类独立排程。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即四类任务都启用（hours 回落默认），
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [9, 21]
	TravelHours    []int // 默认 [9,21]：一趟派出 + 一趟领奖闭环
	ActivityHours  []int // 默认 [10]
	KeepaliveHours []int // 默认 [22]
	BlackcatHours  []int // 默认 [23]：夜猫子（23:00–08:00 计数窗口）
	// GrowthHours 成长任务队列每日自动执行的时点（默认 [1]：凌晨 1 点）。
	// 刻意避开 0 点整：Sequential 小程序任务族在零点解锁下一环，恰好 0 点触发会
	// 抢在解锁写库之前扫描，扫到的还是 locked 形态（growthPending 判为无待办），
	// 白跑一轮；1 点给解锁留出窗口。
	GrowthHours []int

	// ExpiringSoonWindow 快过期积分窗口：签到/余额刷新查余额时，把到期时间
	// <= now+window 的套餐余额标记为"快过期"（pool 据此优先消耗，见
	// entry.creditsExpiring）。<=0 时禁用分桶（全部归长期，行为与引入前一致）。
	// 默认建议 7*24h。
	ExpiringSoonWindow time.Duration

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	// 禁用后不再有任何签到时点。旅行不再搭签到便车（已剥离为独立排程）。
	CheckinDisabled bool
	// TravelDisabled 显式关闭猫猫旅行排程（schedule.travel_enabled=false）。
	TravelDisabled bool
	// ActivityDisabled 显式关闭活跃上报排程（schedule.activity_enabled=false）。
	ActivityDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool
	// BlackcatDisabled 显式关闭夜猫子排程（schedule.blackcat_enabled=false）。
	BlackcatDisabled bool
	// GrowthDisabled 显式关闭成长任务队列自动执行（schedule.growth_enabled=false）。
	// 禁用后 nextWake 不产生 growth 候选，也就没有任何自动点。
	GrowthDisabled bool
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu         sync.Mutex
	adoptTried map[string]string

	// schedMu 保护排程参数（时点/开关）；Reconfigure 可在运行期热改（面板保存配置时调用）。
	// rearmSchedule/rearmBalance 是「排程已变，立即重算」通知：Run 与余额刷新循环各自消费，
	// 分别用独立 channel（同 channel 被两个 select 消费会丢信号）。
	schedMu       sync.Mutex
	rearmSchedule chan struct{}
	rearmBalance  chan struct{}

	// balanceInterval 余额刷新间隔（纳秒，0=暂停）。atomic 读写：执行循环每轮读当前值，
	// SetBalanceInterval 可任意时刻热改（面板保存配置）。
	balanceInterval atomic.Int64

	// growthHook 成长任务队列的执行入口（面板 RunGrowthQueueOnce），由 SetGrowthHook
	// 接线。scheduler 不 import panel（依赖方向单向），到点只回调这个函数值。
	// 由 schedMu 保护：与排程参数一起在运行期可热改。
	growthHook func(ctx context.Context)
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if len(cfg.BlackcatHours) == 0 {
		cfg.BlackcatHours = []int{23}
	}
	if len(cfg.GrowthHours) == 0 {
		cfg.GrowthHours = []int{1}
	}
	return &Scheduler{
		cfg:           cfg,
		adoptTried:    make(map[string]string),
		rearmSchedule: make(chan struct{}, 1),
		rearmBalance:  make(chan struct{}, 1),
	}
}

// Reconfigure 热更新排程参数（面板保存配置后调用）：改时点/开关并通知运行中的循环重算。
// 空 hours 视为「未配置」保留原值（与 config.normalize 的回落语义一致）。
func (s *Scheduler) Reconfigure(checkinHours, travelHours, activityHours, keepaliveHours, blackcatHours []int,
	checkinDisabled, travelDisabled, activityDisabled, keepaliveDisabled, blackcatDisabled bool) {
	s.schedMu.Lock()
	if len(checkinHours) > 0 {
		s.cfg.CheckinHours = checkinHours
	}
	if len(travelHours) > 0 {
		s.cfg.TravelHours = travelHours
	}
	if len(activityHours) > 0 {
		s.cfg.ActivityHours = activityHours
	}
	if len(keepaliveHours) > 0 {
		s.cfg.KeepaliveHours = keepaliveHours
	}
	if len(blackcatHours) > 0 {
		s.cfg.BlackcatHours = blackcatHours
	}
	s.cfg.CheckinDisabled = checkinDisabled
	s.cfg.TravelDisabled = travelDisabled
	s.cfg.ActivityDisabled = activityDisabled
	s.cfg.KeepaliveDisabled = keepaliveDisabled
	s.cfg.BlackcatDisabled = blackcatDisabled
	s.schedMu.Unlock()
	poke(s.rearmSchedule)
	poke(s.rearmBalance)
}

// SetGrowthSchedule 热更新成长任务队列自动执行的时点与开关（面板保存配置后调用）。
// 单独一个方法而不塞进 Reconfigure：Reconfigure 的实参列表已被调用方逐个对位，
// 追加参数会强制所有调用点（含 cmd/server）同步改动；这里零破坏地扩展。
// 空 hours 视为「未配置」保留原值（与 Reconfigure / config.normalize 的回落语义一致）。
func (s *Scheduler) SetGrowthSchedule(hours []int, disabled bool) {
	s.schedMu.Lock()
	if len(hours) > 0 {
		s.cfg.GrowthHours = hours
	}
	s.cfg.GrowthDisabled = disabled
	s.schedMu.Unlock()
	poke(s.rearmSchedule)
}

// SetGrowthHook 接线成长任务队列的执行入口（面板 RunGrowthQueueOnce）。
// 装配期调用即可；运行期调用同样安全（schedMu 保护，正在执行的 hook 不受影响）。
// 未接线（nil）时到点静默跳过——scheduler 包不依赖 panel，接线由 cmd/server 负责。
func (s *Scheduler) SetGrowthHook(fn func(ctx context.Context)) {
	s.schedMu.Lock()
	s.growthHook = fn
	s.schedMu.Unlock()
}

// growthHookFn 锁内读出当前 hook（可能为 nil）。
func (s *Scheduler) growthHookFn() func(context.Context) {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.growthHook
}

// poke 非阻塞发一次唤醒信号（已有待处理信号则忽略，语义等价）。
func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTravel
	taskActivity
	taskKeepalive
	taskBlackcat
	taskGrowth
)

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时（如签到与旅行都含 9），该时刻多类任务需一并执行。
// 已显式禁用的任务不进候选（nextFire 对其零值返回零时间，nextWake 再跳过零时点）。
// 排程参数在 schedMu 下快照，与 Reconfigure 的并发写隔离。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	s.schedMu.Lock()
	checkinHours, keepaliveHours, blackcatHours := s.cfg.CheckinHours, s.cfg.KeepaliveHours, s.cfg.BlackcatHours
	travelHours, activityHours := s.cfg.TravelHours, s.cfg.ActivityHours
	growthHours := s.cfg.GrowthHours
	checkinOff, keepaliveOff, blackcatOff := s.cfg.CheckinDisabled, s.cfg.KeepaliveDisabled, s.cfg.BlackcatDisabled
	travelOff, activityOff := s.cfg.TravelDisabled, s.cfg.ActivityDisabled
	growthOff := s.cfg.GrowthDisabled
	s.schedMu.Unlock()

	type slot struct {
		at   time.Time
		kind taskKind
	}
	var slots []slot
	if !checkinOff {
		slots = append(slots, slot{nextFire(now, checkinHours), taskCheckin})
	}
	if !travelOff {
		slots = append(slots, slot{nextFire(now, travelHours), taskTravel})
	}
	if !activityOff {
		slots = append(slots, slot{nextFire(now, activityHours), taskActivity})
	}
	if !keepaliveOff {
		slots = append(slots, slot{nextFire(now, keepaliveHours), taskKeepalive})
	}
	if !blackcatOff {
		slots = append(slots, slot{nextFire(now, blackcatHours), taskBlackcat})
	}
	// growth 追加在末位：同槽位多族时它的 familyStagger 偏移最大（idx 最大）。
	// Sequential 小程序任务族每日零点解锁一环，默认 01:00 单独成槽（idx 0）无偏移。
	if !growthOff {
		slots = append(slots, slot{nextFire(now, growthHours), taskGrowth})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// wakeupGraceDelay 迟到唤醒补跑的派发前网络宽限：Windows Modern Standby exit 后
// 网络栈/DNS 1-2s 才恢复（issue #152 实测 dial tcp lookup no such host 与
// Kernel-Power 507 standby exit ≤1s 重合），宽限 5s 覆盖 90%+ 唤醒场景。
// 只对迟到补跑生效（准点触发零延迟），零配置（分析报告裁定全套配置不成比例）。
// 测试可缩短（与 travelAccountDelay「测试可置 0」同口径）。
var wakeupGraceDelay = 5 * time.Second

// wakeupLateThreshold 迟到判定阈值：now 晚于槽位计划时刻超过 1s 才算迟到补跑。
// 毫秒级抖动（timer 正常触发的偏移量级）不算，避免准点触发被误宽限。
const wakeupLateThreshold = 1 * time.Second

// awaitWakeupGrace 迟到唤醒补跑派发前的网络宽限：槽位时刻已过点超过阈值
// （机器刚从睡眠唤醒）时先等满 wakeupGraceDelay 让网络栈/DNS 就绪再派发。
// 准点/阈值内抖动零延迟直接放行。ctx 取消立即返回 false（优雅停机不等宽限睡满，
// 本批放弃，下轮 nextWake 照旧从"现在"起算）。返回是否继续派发。
func awaitWakeupGrace(ctx context.Context, planned time.Time) bool {
	if late := time.Since(planned); late <= wakeupLateThreshold {
		return ctx.Err() == nil // 准点触发：零延迟放行
	}
	log.Printf("wakeup grace %s: late catch-up for slot %s", wakeupGraceDelay, planned.Format("15:04"))
	return sleepCtx(ctx, wakeupGraceDelay)
}

// wallclockCheckStep 墙钟校验段长：等待槽位时单次 timer 的最大时长，每段醒来用
// 墙钟重判是否到点。值是「时点精度」与「空闲唤醒频率」的折中——60s 段内时点
// 偏差上限 60s，对签到/保活类任务足够。
const wallclockCheckStep = time.Minute

// slotWake waitSlot 的三态结果。
type slotWake int

const (
	slotFired  slotWake = iota // 墙钟已到达计划时点：补跑本批
	slotRearm                  // 排程已变（Reconfigure）：上层重算下一次唤醒
	slotCancel                 // ctx 取消：上层优雅退出
)

// wallclockNow waitSlot 的墙钟读数源（生产为 time.Now）。每段醒来用它重判是否到点：
// next 由 nextFire 的 time.Date 构造、不携带单调读数，故 next.Sub(wallclockNow()) 是
// 纯墙钟差（睡眠期间墙钟照常前进，判据不落段）。
//
// 声明为变量是 waitSlot 的**测试接缝——勿精简成直接的 time.Now() 调用**。
// 为什么非有不可：本修复的靶子是「单调时钟被系统睡眠冻结」，它在真实时钟下无法复现；
// 而全部走真实时钟时，「分段 + 每段墙钟重判」与「一次性 time.NewTimer(time.Until(next))」
// 在可观测层面完全重合（目标已过点两者都立即返回，目标未到两者都在计划时点返回）——
// scheduler_wait_test.go 里除 TestWaitSlotWallclockRecheckAfterFrozenMonotonic 之外的
// 用例，对**回退成一次性等待的实现同样通过**。只有注入时钟（墙钟照常前进、段计时器按
// 单调时钟"少睡"）才能让落点从「计划时点」变成「计划时点 + 睡眠时长」而证伪。
// 删掉这个变量 = 删掉墙钟修复唯一的证伪手段。生产路径零差异。
var wallclockNow = time.Now

// slotSegment waitSlot 的单段等待（生产为 time.NewTimer）：返回段到期 channel 与停止
// 函数；ctx 取消 / rearm 两条提前返回路径都调用停止函数释放 timer（不泄漏）。
//
// 同样是**测试接缝——勿精简成直接的 time.NewTimer 调用**：测试靠它模拟「段计时器被
// 系统睡眠冻结」（该段按单调时钟等满 d，而墙钟额外前进了睡眠时长），从而让
// TestWaitSlotWallclockRecheckAfterFrozenMonotonic 能区分分段实现与一次性实现。
// 生产路径零差异。
var slotSegment = func(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// waitSlot 分段等待到 next 的**墙钟**时刻（next 由 nextFire 用 time.Date 构造、
// 不携带单调读数，next.Sub(墙钟) 对它是纯墙钟差）。
//
// 为什么不一把 time.NewTimer(time.Until(next)) 睡到底：timer 的等待基于单调时钟，
// macOS / Windows Modern Standby 睡眠会冻结它——睡眠时长不足整个等待时，fire
// 被顺延「睡眠时长」（墙钟已过点、timer 还要继续等），时点被错过且不会立即补跑；
// 睡眠时长超过整个等待时倒是无害的（唤醒瞬间 timer 到期，awaitWakeupGrace 补跑）。
// 分段睡、每段醒来用墙钟重判，把冻结的影响限制在一段之内：睡眠结束后的第一段
// 末尾必然发现「墙钟已越过时点」并立即补跑，偏差上限 = 段长 + 睡眠落段余量。
//
// ctx 取消 / rearmSchedule（在线改配置重排）在每段的 select 里随时返回，段长
// 不影响两者响应性。返回三态见 slotWake。
func (s *Scheduler) waitSlot(ctx context.Context, next time.Time, step time.Duration) slotWake {
	if step <= 0 {
		// 防御：step<=0 会让每段 timer 立即到期而墙钟仍未到点 → 忙等空转。
		step = wallclockCheckStep
	}
	for {
		wallRemain := next.Sub(wallclockNow())
		if wallRemain <= 0 {
			return slotFired
		}
		d := wallRemain
		if d > step {
			d = step
		}
		seg, stop := slotSegment(d)
		select {
		case <-ctx.Done():
			stop()
			return slotCancel
		case <-s.rearmSchedule:
			stop()
			return slotRearm
		case <-seg:
			// 段末回到循环顶用墙钟重判：正常推进时若干段后到点；单调时钟被
			// 睡眠冻结时，墙钟大幅前进，至多一段之后即到点补跑。
		}
	}
}

// schedNow 排程时钟读数源（生产为 time.Now）。声明为变量是**测试接缝**：
// nextFire 的粒度是整点，真实时钟下要验证「到点真的派发了某族」最坏得等到下一个
// 整点（59 分钟）——成长任务自动执行这类「每天到点是否真的动了」的行为，在真实
// 时钟下无法低成本验证。测试把它钉在槽位前一小时，再让 wallclockNow 越过槽位，
// waitSlot 立即 slotFired，无需真实等待。生产路径零差异。
var schedNow = time.Now

// Run 主循环，阻塞直到 ctx 取消。
// Reconfigure 触发 rearmSchedule 时提前唤醒重算（新时点/开关立即生效）。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next, kinds := s.nextWake(schedNow())
		if next.IsZero() {
			// 全部任务族禁用：不空转，等重排通知（在线改配置重新启用）或退出信号。
			select {
			case <-ctx.Done():
				return
			case <-s.rearmSchedule:
				continue
			}
		}
		switch s.waitSlot(ctx, next, wallclockCheckStep) {
		case slotCancel:
			return
		case slotRearm:
			continue // 排程已变：重算下一次唤醒
		case slotFired:
			// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
			// 迟到唤醒（睡眠跨过槽位时刻，timer 在唤醒瞬间才到期）先等网络宽限：
			// 唤醒瞬间 DNS 未就绪，零宽限派发等于把唯一一次补跑机会打在注定失败
			// 的窗口里（issue #152）；准点触发零延迟不受影响。
			if !awaitWakeupGrace(ctx, next) {
				return // ctx 取消：放弃本批，优雅退出
			}
			// 唤醒时全部并行派发：每类一个 goroutine，慢任务族（如活跃上报
			// 多号 × 间隔 ≈ 数分钟睡眠）不再阻塞同槽其他任务族；返回前等全部
			// 任务收尾（下一轮 nextWake 照旧从"现在"起算，多轮重叠的风险与
			// 串行版相同——nextWake 只挑现在之后的时点）。
			s.runBatch(ctx, kinds)
		}
	}
}

// runBatch 并行派发一批任务（同一唤醒时刻的多类任务），等全部完成返回。
// ctx 取消时由各任务内部的可取消等待快速收尾。
func (s *Scheduler) runBatch(ctx context.Context, kinds []taskKind) {
	var wg sync.WaitGroup
	for idx, k := range kinds {
		wg.Add(1)
		go func(k taskKind, idx int) {
			defer wg.Done()
			// 同槽位多任务族**错开启动**（见 familyStagger 注释）：各族都是
			// 「逐账号 + 800ms 限速」的同一节奏，同时启动会锁步推进，导致每个账号
			// 在同一瞬间被两个族各打一次。确定性偏移一次即永久错开。
			if idx > 0 && !sleepCtx(ctx, time.Duration(idx)*familyStagger) {
				return // ctx 取消：不启动本族（优雅停机不必等错开睡醒）
			}
			switch k {
			case taskCheckin:
				s.RunCheckinNow()
			case taskTravel:
				s.RunTravelNow()
			case taskActivity:
				s.runActivity(ctx)
			case taskKeepalive:
				s.RunKeepaliveNow()
			case taskBlackcat:
				s.RunBlackcatNow()
			case taskGrowth:
				s.runGrowth(ctx)
			}
		}(k, idx)
	}
	wg.Wait()
}

// runGrowth 成长任务队列的每日自动执行：回调外部接线的执行入口（面板
// RunGrowthQueueOnce），把「扫描待办 + 执行队列」原样交给面板——队列状态机
// （先占位 / 陈旧占位兜底 / 无待办回滚）只有一份，排程不旁路它。
//
// 为什么用回调而不是 scheduler 直接调 panel：依赖方向单向（scheduler 只依赖
// pool/upstream），接线由 cmd/server 在装配期用 SetGrowthHook 完成；未接线时
// 明确跳过并落一行日志（每天一行的噪音远小于「到点了却什么都没发生」的沉默失败）。
//
// ctx 原样传给 hook：面板队列在项间收尾（优雅停机不等限速睡满）。已在飞行的上游
// 请求无 ctx，取消不能打断它——与手动入口的限制相同。
func (s *Scheduler) runGrowth(ctx context.Context) {
	if ctx.Err() != nil {
		log.Printf("scheduler: 成长任务队列自动执行放弃：%v", ctx.Err())
		return
	}
	hook := s.growthHookFn()
	if hook == nil {
		log.Printf("scheduler: 成长任务队列自动执行未接线（SetGrowthHook 未调用），跳过")
		return
	}
	log.Printf("scheduler: 成长任务队列自动执行到点，交给面板队列入口")
	hook(ctx)
}

// familyStagger 同一槽位内多个任务族之间的启动错开量。
//
// 为什么必须错开（真实缺陷，且**默认配置就命中**）：runBatch 给每个任务族各起一个
// goroutine **并发**执行，而每个族都是「逐账号 + activityAccountDelay(800ms) 限速」
// 的**同一节奏**——同时启动会让它们**锁步推进**：第 1 个账号被两个族在同一瞬间各打
// 一次，第 2 个账号同样如此，全程如此。默认配置 `checkin_hours` 与 `travel_hours`
// 都是 `[9,21]`，即每天 9 点与 21 点各发生一次**系统性 2× 瞬时并发**（不是随机抖动，
// 而是稳定复现的双倍瞬时压力），正是本项目 WAF 403 / 边缘层 fail-fast 一直在防的形态。
//
// 取**确定性偏移**而非随机抖动：同族节奏一致，错开一次即永久错开；确定性还让行为
// 可复现、可测试（随机抖动会让「今天 9 点为什么慢了 3 分钟」无法解释）。
// 偏移量取 5s：远大于单次请求耗时、又远小于最小槽位间隔（1h），不会跨槽。
//
// 声明为 var 是**测试接缝**（与 activityAccountDelay / travelAccountDelay 同口径）：
// 用例把它置 0 以免每批多等数秒。
var familyStagger = 5 * time.Second

// sleepCtx 可取消的等待：ctx 取消立即返回 false（优雅停机不必等限速睡醒），
// 等满返回 true。d<=0 立即放行。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
// 旅行已从签到剥离为独立排程（travel_hours），不再搭签到便车。
// 末尾追加连登管家（streak.go）：可兑换档位自动兑换 + 抽奖次数自动抽完——
// 连登兑换按天数解锁，挂在每日签到后即「到天数那天自动完成兑换→抽奖闭环」。
func (s *Scheduler) RunCheckinNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		// D4 门控：realm=global 账号无签到体系，直接跳过（不发起任何上游调用，避免风控）。
		// 经 auth.Realm() 统一判定：逃生门（global.enabled=false）下 global 账号被降级为 cn、
		// 按 CN 处理——这是逃生门的刻意语义（纯 CN 部署锁死一切 global），与引用处一致。
		if a.IsGlobal() {
			continue
		}
		if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
			// "今天已签到"是幂等成功（上游对重复签到返回 code!=0），不再当失败打 error 行。
			if upstream.IsAlreadyCheckin(err) {
				s.cfg.Pool.NoteCheckinDone(st.UID)
				log.Printf("checkin %s: 今天已签到（幂等）", logfmt.Label(st.UID, st.Nickname))
			} else {
				log.Printf("checkin %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			}
			// 其余业务错误也继续走余额查询
		} else {
			// 首次签到成功此前静默——排查「签到到底跑没跑」时无迹可循（幂等行只在
			// 重复触发时出现），成功也落一行。
			s.cfg.Pool.NoteCheckinDone(st.UID)
			log.Printf("checkin %s: 签到成功", logfmt.Label(st.UID, st.Nickname))
		}
		// 分桶查余额：快过期窗口内的积分单独标记，pool 优先消耗。
		// ExpiringSoonWindow<=0 时退化为纯总量（与引入前一致）。
		//
		// 用 WithExpiry 版拿到**最早到期批次**（earliestAt/earliestRemaining）：只靠
		// 窗口内总量无法判断「谁先过期」，选号优先也就无从谈起。它与旧函数共用同一份
		// 响应体（**零新增上游请求**）——时间戳本就在解析，此前算完求和就丢掉了。
		remain, total, expiring, earliestAt, earliestRemaining, err := s.cfg.Upstream.UserResourceDetailedWithExpiry(a, s.cfg.ExpiringSoonWindow)
		if err != nil {
			log.Printf("user-resource %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			continue
		}
		s.cfg.Pool.ReenableIfCredits(st.UID, remain, total)
		// ⚠️ 顺序不能颠倒：ReenableIfCredits 会清空最早批次快照，必须在它**之后**写入。
		// 且**无条件调用**（不再 `if expiring > 0`）：窗内无到期量时也要把 earliest*
		// 落成零值，否则被 ReenableIfCredits 清掉的快照不会重建。
		s.cfg.Pool.SetCreditsDetailedWithExpiry(st.UID, remain, total, expiring, earliestAt, earliestRemaining)
	}
	s.RunStreakBonusNow()
}

// RunActivityNow 立即对池内所有可用账号执行一次对话活跃上报。
// 禁用账号跳过；无 AccessToken 的跳过；账号间限速 activityAccountDelay。
// 一条上报同时点亮 growth 连登 + 解锁 first_buddy 任务。
// 上报成功后续跑 streak 自检（checkActivityStreak）：回读连登天数，发现
// 「上报 200 但 streak 没涨」的静默丢弃（只读 oracle，不做重试）。
// RunActivityNow 是无 ctx 的外部入口（面板/测试一次性触发）；排程主循环走
// runActivity（ctx 取消时立即放弃剩余账号，不等限速睡满）。
func (s *Scheduler) RunActivityNow() {
	s.runActivity(context.Background())
}

// runActivity 活跃上报遍历，随 ctx 取消立即退出。
func (s *Scheduler) runActivity(ctx context.Context) {
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 门控：global 无任务中心/活跃体系，不发起任何上游调用
		}
		if !first {
			if !sleepCtx(ctx, activityAccountDelay) {
				return // 优雅停机：不等限速睡满，剩余账号下轮再报
			}
		}
		first = false
		cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
		if err := s.cfg.Upstream.ReportChatActivity(a, cid, ""); err != nil {
			log.Printf("activity %s: %v", logfmt.Label(a.UID, a.Nickname), err)
			continue
		}
		s.checkActivityStreak(a) // 上报成功 → 回读 streak 自检
	}
}

// checkActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
// 背景：REPORT-active-map.md §2 实测「上报 200 但静默丢弃」（缺 userId 时 progress 不动），
// 上报 200 ≠ streak 计分——需要回读验证闭环。
// 异常检测口径：days==0 → warn（report OK but streak.days=0 (silent drop?)）；
// GET 失败 → warn 但不影响主流程（上报本身已成功，按天幂等，不做重试）。
// 日志每号一行、一眼可 grep：`activity %s: streak days=%d`（成功也打，方便对账）。
// 返回 true 表示「上报 OK 但 streak 可疑」（days==0 或回读失败），供测试断言。
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.cfg.Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("activity %s: streak check failed (report OK): %v", logfmt.Label(a.UID, a.Nickname), err)
		return true
	}
	if days == 0 {
		log.Printf("activity %s: report OK but streak.days=0 (silent drop?)", logfmt.Label(a.UID, a.Nickname))
		return true
	}
	log.Printf("activity %s: streak days=%d", logfmt.Label(a.UID, a.Nickname), days)
	return false
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
// 12153 禁用走 Pool.NoteSessionDead 的**连续计数**语义：一次刷新失败不再立即杀号，
// 连续 sessionDeadThreshold 次（3 次）才禁用（P0-1：13 个 disabled 号全是历史误判）。
// 刷新成功 → ClearSessionDead 清计数（错误判定的账号有复活路径）。
func (s *Scheduler) RunKeepaliveNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("keepalive %s: 连续 %d 次 12153 session dead — 禁用", logfmt.Label(st.UID, st.Nickname), pool.SessionDeadThreshold())
				}
			}
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID) // 刷新成功清误判计数，失败不该累计
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
		}
	}
}

// RunBalanceRefreshNow 并发对所有非禁用账号查询余额并更新池内 credits。
// 解冻语义与签到一致（ReenableIfCredits：余额 > 0 的冷却账号自动解冻），
// 但不做签到、不刷新 token——只让"积分"这个观测量保持新鲜。
// 供两类入口复用：后台周期任务（StartBalanceRefresh）与面板手动全量刷新。
func (s *Scheduler) RunBalanceRefreshNow() {
	var wg sync.WaitGroup
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(a *auth.Auth, uid string) {
			defer wg.Done()
			remain, total, expiring, earliestAt, earliestRemaining, err := s.cfg.Upstream.UserResourceDetailedWithExpiry(a, s.cfg.ExpiringSoonWindow)
			if err != nil {
				log.Printf("balance %s: %v", uid, err)
				return
			}
			// 保留本处既有的 if/else 语义（有窗内到期量 → Detailed 写入；否则 →
			// ReenableIfCredits 解冻），只把两处换成 WithExpiry 版以带上最早到期批次。
			// 注：本分支里 expiring>0 时**不调** ReenableIfCredits，故不存在
			// 「解冻清空快照 → 覆盖写入」的顺序问题（那是上面签到路径的形状）。
			if expiring > 0 {
				s.cfg.Pool.SetCreditsDetailedWithExpiry(uid, remain, total, expiring, earliestAt, earliestRemaining)
			} else {
				s.cfg.Pool.ReenableIfCredits(uid, remain, total)
			}
		}(a, st.UID)
	}
	wg.Wait()
}

// StartBalanceRefresh 后台周期性余额刷新（独立 ticker goroutine，ctx 取消即停）。
// interval<=0 不启动（schedule.balance_refresh_enabled=false 时 main 不调用即可）。
// 独立于 Run 的小时制排程：余额是分钟级观测量，不值得为它扩展 nextFire 的粒度。
// 运行期可用 SetBalanceInterval 热改间隔（下一轮生效）。
func (s *Scheduler) StartBalanceRefresh(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.balanceInterval.Store(int64(interval))
	go func() {
		var logged time.Duration
		for {
			cur := time.Duration(s.balanceInterval.Load())
			if cur != logged {
				log.Printf("scheduler: 余额后台刷新每 %s（暂停中显示 0s）", cur)
				logged = cur
			}
			if cur <= 0 {
				// 被热改暂停：等重排通知（重新启用时唤醒）或退出。
				select {
				case <-ctx.Done():
					return
				case <-s.rearmBalance:
					continue
				}
			}
			timer := time.NewTimer(cur)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmBalance:
				timer.Stop() // 间隔已变：立刻按新值重算
			case <-timer.C:
				s.RunBalanceRefreshNow()
			}
		}
	}()
}

// SetBalanceInterval 热改余额刷新间隔；<=0 表示暂停循环（面板关闭该开关时）。
func (s *Scheduler) SetBalanceInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.balanceInterval.Store(int64(d))
	poke(s.rearmBalance)
}
