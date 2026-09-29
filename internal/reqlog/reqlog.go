// Package reqlog 提供**请求级**可观测性：内存指标 + 脱敏 JSONL 归档。
//
// 与 internal/usage 的分工：
//   - usage 是**逐请求用量聚合**（按 时间片/域/账号/模型 分桶累计），回答「今天用了多少」；
//   - reqlog 是**逐请求明细**（一次请求一条），回答「刚才那个 5xx 是谁、哪个模型、
//     耗时多少、重试了几次」。聚合数据答不了这类问题——聚合把请求压成了计数器。
//
// # 安全纪律（本包最重要的约束）
//
// 归档**只写请求元数据**，绝不写提示词、响应正文、Authorization、任何凭证、完整 UID。
// 这条纪律靠**结构体字段缺失**保证：Event 里根本不存在能装下内容的字段——除
// time.Time 外全是标量，没有 []byte / map / any / 切片 / 指针 / 嵌套结构。也就是说
// 「写不进内容」是类型系统给的，不是过滤器给的。事后按规则过滤字符串（正则抹
// Authorization、按关键字删 prompt）总会漏：新协议头、新字段名、新编码都可能绕过；
// 而**按构造脱敏**漏不了——除非有人显式往 Event 里加字段，而加字段会被
// security_test.go 的字段白名单闸门拦下（改白名单必须在 review 里被看到）。
//
// # 阻塞纪律
//
// Record/Begin 绝不等待磁盘，也绝不等待归档 goroutine：归档走有界 channel，队列满
// 即丢弃并累加 DroppedWrites。**丢一条日志的代价远小于拖慢一次模型请求**——归档是
// 诊断设施，不能变成请求路径上的故障点。Record 里唯一的锁是保护 100 条环形缓冲的
// O(1) 临界区（无 I/O、无等待）。
//
// # 内存有界
//
// 内存里只留最近 recentCap(100) 条明细，更早的靠计数与归档文件回答，长跑不涨内存。
package reqlog

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 一次请求最终以什么方式结束。
//
//   - success：正常结束（含流式正常收尾）；
//   - http_error：上游返回 >=400（重试耗尽后仍失败）；
//   - stream_error：HTTP 头已 200、流中途断开或解析失败；
//   - interrupted：客户端主动断开（ctx 取消）。
//
// 为什么要显式带 Outcome 而不是只看 Status：后两者与 success 一样都可能是 200，
// 只看状态码无法区分「正常完成」和「吐了一半断线」。面板里 success_rate 与
// http_success_rate 的**差值**就是「HTTP 成功但流失败/被打断」的量——这正是运维
// 最需要看见、而 5xx 计数看不见的那部分故障。
const (
	OutcomeSuccess     = "success"
	OutcomeHTTPError   = "http_error"
	OutcomeStreamError = "stream_error"
	OutcomeInterrupted = "interrupted"
)

const (
	// recentCap 内存里保留的最近请求明细条数上限（Snapshot.Recent 的硬上界）。
	// 100 条足够看清「刚刚发生了什么」，又不随进程运行时长增长。
	recentCap = 100

	// maxPathLen 归档里 Path 的长度上限（去掉 query/fragment 之后再截断）。
	maxPathLen = 256

	// 配置零值回落（见 Config 各字段注释）。
	defaultRetentionDays = 7
	defaultMaxBytes      = 100 << 20 // 100 MiB：整个归档目录的总量上限
	defaultFileMaxBytes  = 16 << 20  // 16 MiB：单个归档文件上限
	defaultQueueSize     = 1024      // 归档队列容量

	// requestIDBytes 请求 ID 的随机字节数：8 字节 → 16 hex。
	requestIDBytes = 8
)

// Config 归档与指标配置。零值即「只开内存指标，不落盘」。
type Config struct {
	Dir           string // 归档目录；空 或 Enabled=false → 只启用内存指标，不落盘
	Enabled       bool
	RetentionDays int   // <=0 回落 7
	MaxBytes      int64 // <=0 回落 100<<20（整个归档目录的总量上限）
	FileMaxBytes  int64 // <=0 回落 16<<20（单文件上限，超出即换下一个序号）
	QueueSize     int   // <=0 回落 1024（归档队列容量，满即丢弃）
}

// Event 是一条**脱敏**请求记录。
//
// 这里没有、也永远不该有 prompt / body / headers / response / authorization 之类
// 字段：Event 是归档的完整写入面，它的字段集合就是脱敏契约本身。新增任何字段都
// 会打破 security_test.go 的闸门，必须先说明「它为什么装不下请求内容」。
//
// Account 只保存「昵称(uid8)」标签（如 "猫(a1b2c3d4)"，由 internal/logfmt.Label 产出），
// **不保存完整 UID**：uid8 足够在日志/state.json 里反查定位，完整 UID 是账号标识，
// 归档里不该出现。
type Event struct {
	Time             time.Time `json:"time"`
	RequestID        string    `json:"request_id"`
	Path             string    `json:"path"`
	Account          string    `json:"account,omitempty"`
	Model            string    `json:"model,omitempty"`
	Status           int       `json:"status"`
	OK               bool      `json:"ok"`
	Outcome          string    `json:"outcome"`
	DurationMs       int64     `json:"duration_ms"`
	TTFBMs           int64     `json:"ttfb_ms,omitempty"`
	Attempts         int       `json:"attempts,omitempty"`
	PromptTokens     int64     `json:"prompt_tokens,omitempty"`
	CompletionTokens int64     `json:"completion_tokens,omitempty"`
	TotalTokens      int64     `json:"total_tokens,omitempty"`
	Credit           float64   `json:"credit,omitempty"`
	// HasCredit 区分「上游明确给了 0 额度」与「没有观测」——Credit 是 float64，
	// 0 值无法自证来源，所以显式带一个已知性标记（且不带 omitempty：它必须总在）。
	HasCredit bool `json:"credit_known"`
}

// Snapshot 是面板一次拉取的全部请求级视图数据。
type Snapshot struct {
	StartedAt       time.Time    `json:"started_at"`
	Completed       int64        `json:"completed"`
	InFlight        int64        `json:"in_flight"`
	Succeeded       int64        `json:"succeeded"`
	Failed          int64        `json:"failed"`
	SuccessRate     float64      `json:"success_rate"`
	HTTPSuccessRate float64      `json:"http_success_rate"`
	AvgDurationMs   float64      `json:"avg_duration_ms"`
	Recent          []Event      `json:"recent"`
	Archive         ArchiveStats `json:"archive"`
}

// ArchiveStats 归档侧的健康状况。DroppedWrites 是「归档漏了多少条」的唯一口径：
// 面板看到它非 0，就说明这段时间的明细有缺口，别把 Recent 当成完整历史。
type ArchiveStats struct {
	Enabled       bool   `json:"enabled"`
	Dir           string `json:"dir,omitempty"`
	Files         int    `json:"files"`
	Bytes         int64  `json:"bytes"`
	DroppedWrites uint64 `json:"dropped_writes"`
	LastError     string `json:"last_error,omitempty"`
}

// Recorder 并发安全的请求级记录器。
//
// 锁的划分（Record 在请求路径上，必须最短）：
//   - 计数器：atomic，无锁；
//   - ring/ringAt/ringN：一个互斥锁，临界区是「拷一个 Event + 改三个整数」；
//   - 归档文件：fileMu，只在归档 goroutine 与 Cleanup 之间争用，**请求路径永不触碰**。
type Recorder struct {
	cfg     Config
	dir     string
	enabled bool

	started time.Time

	inFlight  atomic.Int64
	completed atomic.Int64
	succeeded atomic.Int64
	httpOK    atomic.Int64
	durSumMs  atomic.Int64

	mu     sync.Mutex
	ring   []Event // 定长环形缓冲，长度恒为 recentCap
	ringAt int     // 下一个写入位置
	ringN  int     // 有效条数（<= recentCap）

	queue   chan Event
	dropped atomic.Uint64

	fileMu   sync.Mutex
	curFile  *os.File
	curName  string
	curDate  string
	curSeq   int
	curBytes int64

	errMu   sync.Mutex
	lastErr string

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// New 创建记录器并（在启用归档时）启动归档 goroutine。
//
// 只有 Enabled 为真**且** Dir 非空才落盘；否则纯内存，Snapshot 仍能给出指标与最近
// 请求，只是 Archive.Enabled 为 false、不产生任何文件。
//
// 注意：New **不**做清理。删旧归档是 Cleanup 的职责（Lead 会定期调），这样 New 保持
// 「只建目录、不删文件」的可预期语义，避免调用方刚放进目录的文件被意外清掉。
func New(cfg Config) *Recorder {
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = defaultRetentionDays
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultMaxBytes
	}
	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMaxBytes
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueueSize
	}

	r := &Recorder{
		cfg:     cfg,
		started: time.Now(),
		ring:    make([]Event, recentCap),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}

	if !cfg.Enabled || strings.TrimSpace(cfg.Dir) == "" {
		// 未启用归档：done 预先关闭，Close 立即返回，不启动 goroutine。
		close(r.done)
		return r
	}

	r.enabled = true
	r.dir = cfg.Dir
	r.queue = make(chan Event, cfg.QueueSize)
	if err := r.initArchive(); err != nil {
		// 建不出目录**不**降级成「内存模式」：配置要的是落盘，失败必须能从
		// Archive.LastError 看见，而不是静默地什么都不记（静默失败比失败更难查）。
		// 后续每次写盘都会重试并刷新 LastError。
		r.setErr(err)
	}
	go r.run()
	return r
}

// NewRequestID 生成一条本地请求 ID，形如 "req-<16 hex>"。
//
// 只用于把一次请求的日志行、归档行、错误响应串起来，**不含任何用户/账号信息**：
// 它是纯随机值，不派生自 uid、会话键或请求体，因此无法被反查出账号或内容。
// crypto/rand 失败时回落时间戳（同形 16 hex），绝不 panic、绝不返回空串——
// 熵源故障不该把请求打挂。
func NewRequestID() string {
	var b [requestIDBytes]byte
	if _, err := rand.Read(b[:]); err == nil {
		return "req-" + hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("req-%016x", uint64(time.Now().UnixNano()))
}

// Begin 记一次请求进入（InFlight++）。无锁、非阻塞，可在请求路径任意位置调用。
func (r *Recorder) Begin() {
	if r == nil {
		return
	}
	r.inFlight.Add(1)
}

// Record 记一次请求结束。**非阻塞**：不等待磁盘、不等待归档 goroutine，队列满即
// 丢弃并累加 DroppedWrites。
//
// 顺序上先把内存计数落定，再入队归档：归档可能被丢弃或写失败，但面板的
// Completed/Succeeded/Failed 必须完整——「归档漏了几条」由 DroppedWrites 单独回答，
// 不能因为丢日志就让成功率失真。
func (r *Recorder) Record(ev Event) {
	if r == nil {
		return
	}

	// 规范化：Time 缺失补当前时间；Path 收敛成纯路由（见 sanitizePath）。
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	if ev.Path != "" {
		ev.Path = sanitizePath(ev.Path)
	}

	r.completed.Add(1)
	if ev.OK {
		r.succeeded.Add(1)
	}
	// HTTP 层成功只看状态码：2xx/3xx。流式请求「头 200 但吐一半断线」在这里算
	// HTTP 成功、在 OK/Succeeded 里算失败——两个口径的差就是这类故障的量。
	if ev.Status >= 200 && ev.Status < 400 {
		r.httpOK.Add(1)
	}
	if ev.DurationMs > 0 {
		r.durSumMs.Add(ev.DurationMs)
	}

	// InFlight 只减不增；Begin/Record 不成对（调用方 bug）时夹到 0，不让负数一路
	// 漂下去把读数带偏。用 CAS 循环而不是 Add(-1) 后 Store(0)：后者会与并发的
	// Begin 抢写，把一次真实的进入计数抹掉。
	for {
		cur := r.inFlight.Load()
		if cur <= 0 {
			break
		}
		if r.inFlight.CompareAndSwap(cur, cur-1) {
			break
		}
	}

	r.mu.Lock()
	r.ring[r.ringAt] = ev
	r.ringAt = (r.ringAt + 1) % recentCap
	if r.ringN < recentCap {
		r.ringN++
	}
	r.mu.Unlock()

	if r.queue != nil {
		select {
		case r.queue <- ev:
		default:
			// 队列满：丢弃而不是等待。这是本包最重要的一个分支。
			r.dropped.Add(1)
		}
	}
}

// Snapshot 产出面板读取的当前视图。
//
// limit 是 Recent 的条数上限：<=0 表示「全部」（上界仍是 recentCap=100），
// >recentCap 会被夹到 recentCap。Recent 按完成时间**倒序**（最新一条在 [0]）。
//
// 计数与 Recent 分别取自 atomic 与环形缓冲，因此快照在并发下是「各字段各自精确、
// 整体可能有极小的时序错位」——对监控读数而言这个弱一致性是刻意的取舍：为了它去
// 加一把全局锁会把锁带上请求路径。
func (r *Recorder) Snapshot(limit int) Snapshot {
	if r == nil {
		return Snapshot{Recent: []Event{}}
	}

	completed := r.completed.Load()
	succeeded := r.succeeded.Load()
	failed := completed - succeeded
	if failed < 0 {
		failed = 0
	}
	inFlight := r.inFlight.Load()
	if inFlight < 0 {
		inFlight = 0
	}

	snap := Snapshot{
		StartedAt: r.started,
		Completed: completed,
		InFlight:  inFlight,
		Succeeded: succeeded,
		Failed:    failed,
		Recent:    r.recent(limit),
		Archive:   r.archiveStats(),
	}
	if completed > 0 {
		snap.SuccessRate = float64(succeeded) / float64(completed)
		snap.HTTPSuccessRate = float64(r.httpOK.Load()) / float64(completed)
		// 分母是全部完成请求：没报耗时的请求按 0 计入（调用方不报 = 无观测）。
		snap.AvgDurationMs = float64(r.durSumMs.Load()) / float64(completed)
	}
	return snap
}

// recent 返回最近 n 条（倒序）。n 由 limit 与已存条数共同决定。
func (r *Recorder) recent(limit int) []Event {
	if limit <= 0 || limit > recentCap {
		limit = recentCap
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.ringN
	if n > limit {
		n = limit
	}
	out := make([]Event, 0, n)
	for i := 0; i < n; i++ {
		// 最新一条在 ringAt-1；i 最大 recentCap-1，故一次补加 recentCap 即可回正。
		idx := r.ringAt - 1 - i
		if idx < 0 {
			idx += recentCap
		}
		out = append(out, r.ring[idx])
	}
	return out
}

// Close 停止归档 goroutine 并关闭当前文件。**幂等**，重复调用安全。
//
// 停止前会排空队列（上界 = 队列容量），尽量不丢已入队的事件。Close 之后 Record
// 仍然安全：事件照旧入队，只是没人消费，队列满即丢弃——**不 panic、不阻塞**。
// 这一点是刻意的：归档停摆（崩溃/卡死/已关闭）绝不能反过来拖住模型请求。
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		close(r.stop)
		<-r.done
	})
}

// sanitizePath 把请求路径收敛成纯路由：去掉 query 与 fragment，再按 maxPathLen 截断。
//
// 这是**结构脱敏之外的兜底**，不是主要防线：Event 里本来就没有内容字段，这里防的是
// 「调用方把敏感串塞进了 Path 本身」——客户端可以把 key 放在 query 里
// （/v1/chat/completions?api_key=sk-...），路径本身就成了凭证载体。
func sanitizePath(p string) string {
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if len(p) > maxPathLen {
		p = p[:maxPathLen]
	}
	return p
}
