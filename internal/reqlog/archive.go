// archive.go 脱敏 JSONL 归档：按天切分、单文件限容、按保留期/总量清理。
//
// 写入模型：**单写者**。所有写盘都由 New 启动的那一个归档 goroutine 完成
// （Recorder.run），Cleanup 由外部定期调用、只与它争 fileMu。请求 goroutine 永远
// 只做一次非阻塞入队，碰不到文件句柄——这是「归档不拖慢请求」的结构保证。
//
// 文件名约定：requests-YYYY-MM-DD.jsonl，同一天写满 FileMaxBytes 后递增序号为
// requests-YYYY-MM-DD.1.jsonl、.2.jsonl……日期用本地时区（与运维直觉一致，也和
// internal/usage 的日桶口径相同）。**只有符合该约定的文件才会被 Cleanup 删除**，
// 归档目录里别的东西一律不碰。
package reqlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

const (
	// dayLayout 归档文件名里的日期格式；YYYY-MM-DD 的字典序与时间序一致，
	// 因此清理逻辑可以直接比字符串，不必解析时间。
	dayLayout = "2006-01-02"

	archivePrefix = "requests-"
	archiveExt    = ".jsonl"
)

// archiveNameRE 匹配归档文件命名：requests-2026-01-02.jsonl / requests-2026-01-02.3.jsonl。
// 捕获组 1 是日期、组 2 是可选序号。
var archiveNameRE = regexp.MustCompile(`^` + archivePrefix + `(\d{4}-\d{2}-\d{2})(?:\.(\d+))?` + regexp.QuoteMeta(archiveExt) + `$`)

// archiveFile 归档目录里一个可识别的归档文件。
type archiveFile struct {
	name string
	date string // YYYY-MM-DD
	seq  int    // 同一天的第几个文件（0 是当天第一个）
	size int64
}

// archiveName 拼出归档文件名。seq<=0 即当天第一个文件。
func archiveName(date string, seq int) string {
	if seq <= 0 {
		return archivePrefix + date + archiveExt
	}
	return fmt.Sprintf("%s%s.%d%s", archivePrefix, date, seq, archiveExt)
}

// listArchive 列出 dir 下所有符合命名约定的归档文件，按「最旧在前」排序
// （日期升序，同日期序号升序）。命名之外的文件与子目录一律忽略。
//
// 大小一律走 os.Stat，不用 DirEntry.Info()：**Windows 上被打开写入的文件，
// 目录枚举返回的大小是陈旧的**（实测：文件已写入 155 字节，DirEntry.Info().Size()
// 仍报 0，文件句柄关闭后才变正确）。用陈旧大小会让 Cleanup 的容量核算少算、
// 删不够，也会让面板的 Archive.Bytes 长期偏低。目录里只有个位数到几十个条目，
// 多几十次 Stat 换正确性是划算的。
func listArchive(dir string) ([]archiveFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]archiveFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := archiveNameRE.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		fi, err := os.Stat(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		seq := 0
		if m[2] != "" {
			if v, err := strconv.Atoi(m[2]); err == nil {
				seq = v
			}
		}
		out = append(out, archiveFile{name: e.Name(), date: m[1], seq: seq, size: fi.Size()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].date != out[j].date {
			return out[i].date < out[j].date
		}
		return out[i].seq < out[j].seq
	})
	return out, nil
}

// initArchive 建归档目录。失败只记录、不阻断：Recorder 仍可提供内存指标。
func (r *Recorder) initArchive() error {
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return fmt.Errorf("建归档目录 %s: %w", r.dir, err)
	}
	return nil
}

// run 归档 goroutine：串行消费队列，直到 Close。
func (r *Recorder) run() {
	// defer 顺序：先关文件再 close(done)，保证 Close 返回时文件已经 flush 并关闭。
	defer close(r.done)
	defer r.closeFile()

	for {
		select {
		case ev := <-r.queue:
			r.write(ev)
		case <-r.stop:
			// 停机排空：把已入队的事件尽量落盘（上界 = 队列容量，不会无限循环）。
			for {
				select {
				case ev := <-r.queue:
					r.write(ev)
				default:
					return
				}
			}
		}
	}
}

// write 序列化并追加一条事件。任何失败都计入 DroppedWrites——面板关心的是
// 「归档漏了多少条」，至于是队列满漏的还是写盘失败漏的，对使用者是一回事。
func (r *Recorder) write(ev Event) {
	line, err := json.Marshal(ev)
	if err != nil {
		r.setErr(fmt.Errorf("序列化事件 %s: %w", ev.RequestID, err))
		r.dropped.Add(1)
		return
	}
	line = append(line, '\n')
	if err := r.append(line, ev.Time); err != nil {
		r.setErr(err)
		r.dropped.Add(1)
	}
}

// append 把一行追加进归档文件（必要时先换文件）。
func (r *Recorder) append(line []byte, ts time.Time) error {
	r.fileMu.Lock()
	defer r.fileMu.Unlock()

	if err := r.ensureFileLocked(ts, int64(len(line))); err != nil {
		return err
	}
	if _, err := r.curFile.Write(line); err != nil {
		return fmt.Errorf("写归档 %s: %w", r.curName, err)
	}
	r.curBytes += int64(len(line))
	return nil
}

// ensureFileLocked 保证 curFile 可写，需要时换文件。
//
// 换文件的两种原因：
//   - 日期变了（跨天切分）；
//   - 当前文件加上这一行会超 FileMaxBytes。
//
// 单行本身就大于 FileMaxBytes 时允许空文件超限写入（lineLen > max 时 curBytes==0
// 也放行）：否则要么丢这行，要么写出 0 字节文件，两种都更糟。
func (r *Recorder) ensureFileLocked(ts time.Time, n int64) error {
	date := ts.Format(dayLayout)
	if r.curFile != nil && r.curDate == date && (r.curBytes == 0 || r.curBytes+n <= r.cfg.FileMaxBytes) {
		return nil
	}

	r.closeFileLocked()
	if r.curDate != date {
		r.curDate = date
		r.curSeq = 0
	} else {
		// 同日因容量换文件：序号递增，绝不覆盖已写满的文件。
		r.curSeq++
	}
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return fmt.Errorf("建归档目录 %s: %w", r.dir, err)
	}

	// 上次进程退出时可能已经留下同名文件（同一天重启）：跳到第一个放得下的序号，
	// 继续追加而不是覆盖。循环必然终止——同名文件数量有限，总有一个序号不存在
	// （大小 0，必然放得下）。
	for {
		name := archiveName(date, r.curSeq)
		f, err := os.OpenFile(filepath.Join(r.dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("打开归档 %s: %w", name, err)
		}
		fi, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return fmt.Errorf("归档 %s 取状态: %w", name, err)
		}
		if fi.Size() > 0 && fi.Size()+n > r.cfg.FileMaxBytes {
			_ = f.Close()
			r.curSeq++
			continue
		}
		r.curFile = f
		r.curName = name
		r.curBytes = fi.Size()
		return nil
	}
}

// closeFileLocked 关闭当前文件（调用方须持 fileMu）。
func (r *Recorder) closeFileLocked() {
	if r.curFile != nil {
		_ = r.curFile.Close()
		r.curFile = nil
	}
	r.curName = ""
	r.curBytes = 0
}

// closeFile 关闭当前文件（自己取锁）。
func (r *Recorder) closeFile() {
	r.fileMu.Lock()
	defer r.fileMu.Unlock()
	r.closeFileLocked()
}

// Cleanup 按 RetentionDays 与 MaxBytes 清理归档目录。由调用方定期调用（Lead 负责）。
//
// 两轮删除，先天数后容量：
//  1. 天数：保留 [今天-(RetentionDays-1), 今天] 共 RetentionDays 天，更早的整日
//     文件直接删（RetentionDays=1 即只留今天）。
//  2. 容量：总量超过 MaxBytes 时从**最旧**开始删，直到不超。
//
// 当前正在写的文件**永不删除**：Windows 上删打开中的文件本来就会失败，而且当
// MaxBytes 小于单文件上限时，显式跳过可保证「配置过小」不会把归档写挂（宁可超限，
// 也不能边写边删自己的文件）。
//
// 只动符合 requests-YYYY-MM-DD[.N].jsonl 的文件；目录里的其他内容一律不碰。
func (r *Recorder) Cleanup() {
	if r == nil || !r.enabled {
		return
	}
	r.fileMu.Lock()
	defer r.fileMu.Unlock()

	files, err := listArchive(r.dir)
	if err != nil {
		if !os.IsNotExist(err) {
			r.setErr(fmt.Errorf("读归档目录 %s: %w", r.dir, err))
		}
		return
	}

	cutoff := time.Now().AddDate(0, 0, -(r.cfg.RetentionDays - 1)).Format(dayLayout)

	// 就地过滤：写指针恒不超前于读指针，标准写法、零额外分配。
	kept := files[:0]
	var total int64
	for _, f := range files {
		if f.name != r.curName && f.date < cutoff {
			if err := os.Remove(filepath.Join(r.dir, f.name)); err != nil {
				r.setErr(fmt.Errorf("删过期归档 %s: %w", f.name, err))
				kept = append(kept, f)
				total += f.size
			}
			continue
		}
		kept = append(kept, f)
		total += f.size
	}

	for i := 0; i < len(kept) && total > r.cfg.MaxBytes; i++ {
		f := kept[i]
		if f.name == r.curName {
			continue
		}
		if err := os.Remove(filepath.Join(r.dir, f.name)); err != nil {
			r.setErr(fmt.Errorf("删超额归档 %s: %w", f.name, err))
			continue
		}
		total -= f.size
	}
}

// archiveStats 汇总归档现状（文件数/字节数/丢弃数/最近错误）供面板展示。
//
// 文件数与字节数每次扫描目录得出（保留 7 天、单文件 16 MiB 的量级下目录只有个位数
// 到几十个条目，ReadDir 是微秒级）；不缓存是刻意的——运维手工删过文件、或外部
// 脚本归档过之后，面板显示的必须仍是真实磁盘状态，而不是一份可能过期的记忆。
func (r *Recorder) archiveStats() ArchiveStats {
	st := ArchiveStats{Enabled: r.enabled, DroppedWrites: r.dropped.Load()}
	if r.enabled {
		st.Dir = r.dir
		r.fileMu.Lock()
		files, err := listArchive(r.dir)
		r.fileMu.Unlock()
		if err != nil {
			if !os.IsNotExist(err) {
				r.setErr(fmt.Errorf("读归档目录 %s: %w", r.dir, err))
			}
		} else {
			st.Files = len(files)
			for _, f := range files {
				st.Bytes += f.size
			}
		}
	}
	st.LastError = r.lastError()
	return st
}

// setErr 记录最近一次归档错误（保留最早的表象不覆盖？不——保留**最新**的，
// 面板看到的是「现在为什么漏」，不是「历史上第一次为什么漏」）。
func (r *Recorder) setErr(err error) {
	if err == nil {
		return
	}
	r.errMu.Lock()
	r.lastErr = err.Error()
	r.errMu.Unlock()
}

// lastError 返回最近一次归档错误（无则空串）。
func (r *Recorder) lastError() string {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	return r.lastErr
}
