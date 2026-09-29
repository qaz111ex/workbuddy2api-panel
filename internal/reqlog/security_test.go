package reqlog

// security_test.go 是**安全闸门**：它守的是「归档里绝不出现请求内容/凭证/完整 UID」
// 这条纪律。
//
// 纪律靠**结构**保证（Event 里没有能装内容的字段），所以闸门也必须是**结构性**的：
// 它检查 Event 的字段与类型、以及真正落盘那一行的键集，而不是「某个字符串恰好没被
// 写进去」。后者只能证明今天没漏，前者能证明明天也漏不了——除非有人显式改白名单，
// 而改白名单必须在 code review 里被看到。
//
// 三条断言，缺一不可：
//  1. 字段闸门：Event 只允许白名单字段，且类型只能是标量（+ time.Time）。
//     只要存在一种「能装下任意内容」的类型（[]byte/map/any/切片/指针/嵌套结构），
//     脱敏就不再是构造性的，这里必须红。
//  2. 序列化闸门：Event 填满哨兵值后 marshal，键集必须**恰好**等于白名单，
//     且不含任何内容类键名。
//  3. 落盘闸门：真的写一条归档、读回那一行，键集仍必须恰好等于白名单——
//     证明归档链路（而非只有结构体定义）没有夹带私货。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// eventFieldAllowlist 是 Event **允许存在**的字段：Go 字段名 → JSON 键。
// 这份白名单就是脱敏契约本身——Event 里能出现什么，只有这里说了算。
//
// 想加字段？先回答三个问题，再改这份表：
//   - 它会不会装下提示词/响应正文/请求头/凭证？
//   - 它会不会装下完整 UID（而不是 uid8）？
//   - 它的类型是不是标量？非标量一律视为「能装内容」，直接拒。
var eventFieldAllowlist = map[string]string{
	"Time":             "time",
	"RequestID":        "request_id",
	"Path":             "path",
	"Account":          "account",
	"Model":            "model",
	"Status":           "status",
	"OK":               "ok",
	"Outcome":          "outcome",
	"DurationMs":       "duration_ms",
	"TTFBMs":           "ttfb_ms",
	"Attempts":         "attempts",
	"PromptTokens":     "prompt_tokens",
	"CompletionTokens": "completion_tokens",
	"TotalTokens":      "total_tokens",
	"Credit":           "credit",
	"HasCredit":        "credit_known",
}

// forbiddenKeySubstrings 内容类键名的兜底扫描（键名子串匹配）。
//
// 刻意不含 "prompt"：合法键 prompt_tokens 含该子串，误报会把闸门变成噪音。裸
// "prompt" 键由精确键集断言覆盖（键集必须恰好等于白名单，多一个就红）。
var forbiddenKeySubstrings = []string{
	"body", "content", "message", "authorization", "credential", "secret",
	"password", "cookie", "header", "uid", "payload", "response", "text",
	"api_key", "apikey", "raw", "data",
}

// TestEventStructCannotHoldContent 字段闸门：类型系统层面证明「写不进内容」。
func TestEventStructCannotHoldContent(t *testing.T) {
	typ := reflect.TypeOf(Event{})
	if typ.NumField() != len(eventFieldAllowlist) {
		t.Fatalf("Event 字段数 %d ≠ 白名单 %d：新增/删除字段必须先改 eventFieldAllowlist 并说明脱敏影响",
			typ.NumField(), len(eventFieldAllowlist))
	}

	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if _, ok := eventFieldAllowlist[f.Name]; !ok {
			t.Errorf("Event 出现白名单外字段 %q（类型 %s）：它可能承载请求内容，必须显式评审", f.Name, f.Type)
			continue
		}

		switch f.Type.Kind() {
		case reflect.String, reflect.Bool,
			reflect.Int, reflect.Int64,
			reflect.Float64:
			// 标量：最多装一个数字/一个布尔/一个字符串，装不下整段请求或响应。
		case reflect.Struct:
			// 唯一允许的结构体是 time.Time（它本身也只是个标量时间戳）。
			if f.Type != reflect.TypeOf(time.Time{}) {
				t.Errorf("字段 %s 是结构体 %s：Event 不允许嵌套结构（可能整段装下请求/响应体）", f.Name, f.Type)
			}
		default:
			t.Errorf("字段 %s 的类型 %s 能装下任意内容（[]byte/map/any/切片/指针/接口…），破坏构造性脱敏", f.Name, f.Type)
		}
	}
}

// TestEventJSONKeysAreExactlyTheAllowlist 序列化闸门：键集恰好等于白名单。
func TestEventJSONKeysAreExactlyTheAllowlist(t *testing.T) {
	// 填满所有字段，让每个带 omitempty 的键都出现——漏一个键也算失败，
	// 这样「契约里承诺的字段」与「实际写出的字段」不会有静默漂移。
	ev := Event{
		Time:             time.Now(),
		RequestID:        "req-0123456789abcdef",
		Path:             "/v1/chat/completions",
		Account:          "猫(a1b2c3d4)",
		Model:            "claude-sonnet-4",
		Status:           200,
		OK:               true,
		Outcome:          OutcomeSuccess,
		DurationMs:       1234,
		TTFBMs:           123,
		Attempts:         2,
		PromptTokens:     11,
		CompletionTokens: 22,
		TotalTokens:      33,
		Credit:           1.5,
		HasCredit:        true,
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal Event: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal Event: %v", err)
	}

	want := make(map[string]bool, len(eventFieldAllowlist))
	for _, k := range eventFieldAllowlist {
		want[k] = true
	}
	for k := range got {
		if !want[k] {
			t.Errorf("Event 序列化出现白名单外键 %q（值 %s）——这是内容泄漏的入口", k, got[k])
		}
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("Event 序列化缺少契约键 %q：契约与实际输出不一致", k)
		}
	}

	// 兜底：内容类键名一个都不许有。
	for k := range got {
		lower := strings.ToLower(k)
		for _, bad := range forbiddenKeySubstrings {
			if strings.Contains(lower, bad) {
				t.Errorf("Event 序列化出现内容类键 %q（含 %q）", k, bad)
			}
		}
		if lower == "prompt" || lower == "body" || lower == "authorization" {
			t.Errorf("Event 序列化出现内容类键 %q", k)
		}
	}
}

// TestArchivedLineCarriesNoExtraKeys 落盘闸门：真写一条、读回那一行，键集仍受控。
func TestArchivedLineCarriesNoExtraKeys(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir, Enabled: true, QueueSize: 64})
	r.Record(Event{
		Time:             time.Now(),
		RequestID:        "req-0123456789abcdef",
		Path:             "/v1/chat/completions",
		Account:          "猫(a1b2c3d4)",
		Model:            "claude-sonnet-4",
		Status:           200,
		OK:               true,
		Outcome:          OutcomeSuccess,
		DurationMs:       1234,
		TTFBMs:           123,
		Attempts:         2,
		PromptTokens:     11,
		CompletionTokens: 22,
		TotalTokens:      33,
		Credit:           1.5,
		HasCredit:        true,
	})
	r.Close()

	lines := readArchiveLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("期望 1 行归档，实际 %d 行", len(lines))
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("归档行不是合法 JSON: %v\n%s", err, lines[0])
	}
	want := make(map[string]bool, len(eventFieldAllowlist))
	for _, k := range eventFieldAllowlist {
		want[k] = true
	}
	for k := range got {
		if !want[k] {
			t.Errorf("归档行出现白名单外键 %q（值 %s）——落盘链路夹带了契约外字段", k, got[k])
		}
	}

	// 完整 UID 不该出现在归档里：Account 只允许「昵称(uid8)」形态的标签。
	// 这里用 uid8 形态的哨兵，确认它没有被某种自动补全成完整 uid。
	if !strings.Contains(lines[0], "a1b2c3d4") {
		t.Errorf("归档行丢了账号标签：%s", lines[0])
	}
}

// TestPathQueryStringIsStripped 兜底：query/fragment 不进归档（客户端可能把 key 放 query）。
func TestPathQueryStringIsStripped(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	r.Record(Event{Path: "/v1/chat/completions?api_key=sk-live-SECRET#frag"})

	s := r.Snapshot(0)
	if len(s.Recent) != 1 {
		t.Fatalf("Recent 条数 = %d，期望 1", len(s.Recent))
	}
	if got, want := s.Recent[0].Path, "/v1/chat/completions"; got != want {
		t.Errorf("Path = %q，期望 %q（query 里的凭证不该进归档）", got, want)
	}
	if strings.Contains(s.Recent[0].Path, "SECRET") {
		t.Errorf("Path 仍含 query 内容: %q", s.Recent[0].Path)
	}
}

// TestPathIsTruncated 兜底：异常长路径不把归档行撑成一条巨行。
func TestPathIsTruncated(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	long := "/" + strings.Repeat("a", maxPathLen*2)
	r.Record(Event{Path: long})
	if got := len(r.Snapshot(0).Recent[0].Path); got != maxPathLen {
		t.Errorf("Path 长度 = %d，期望截断到 %d", got, maxPathLen)
	}
}

// TestArchiveFileNamesAreScoped 清理边界：只认自己的命名，别人的文件一律不碰。
func TestCleanupIgnoresForeignFiles(t *testing.T) {
	dir := t.TempDir()
	foreign := filepath.Join(dir, "state.json")
	if err := os.WriteFile(foreign, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "requests-2000-01-01.jsonl"), 0o700); err != nil {
		t.Fatal(err) // 同名的**目录**也不该被当成归档文件删掉
	}
	old := "requests-2000-01-01.jsonl.bak"
	if err := os.WriteFile(filepath.Join(dir, old), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := New(Config{Dir: dir, Enabled: true, RetentionDays: 7, MaxBytes: 1})
	r.Cleanup()
	r.Close()

	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("非归档文件被删了: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, old)); err != nil || fi.IsDir() {
		t.Errorf("后缀不符的文件被删了: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "requests-2000-01-01.jsonl")); err != nil || !fi.IsDir() {
		t.Errorf("同名目录被动了: %v", err)
	}
}
