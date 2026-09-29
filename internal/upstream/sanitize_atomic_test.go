// sanitize_atomic_test.go 钉住「面板热改 × chat 热路径」的脱敏开关数据竞争修复
// （吸收上游 5f6c7ca）：开关必须是 atomic.Bool，且每次请求 Load 一次（热改立即生效）。
//
// 本机无 gcc、`go test -race` 不可用，故竞争消除的论证分两层：
//  1. 类型层（TestSanitizeFingerprintsFieldIsAtomic + ...GuardHasTeeth）：
//     atomic.Bool 内部只有一个不导出的 v 字段，外部无法绕过 Load/Store 读写；
//     字段一旦退回普通 bool，守卫用例立即失败——这就是「有牙」的反事实验证。
//  2. 行为层（...HotReloadEffective）：同一个 Client 先 Store(false) 再 Store(true)，
//     两次出站 body 必须不同——证明读的是每次请求的 Load 值而非构造时的副本。
package upstream

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// sanitizeFlagTypeErr 报告 v 的 SanitizeFingerprints 字段不是 atomic.Bool。
// 独立成函数是为了让「守卫有牙」可以被反事实验证（拿普通 bool 持有者喂进来）。
func sanitizeFlagTypeErr(v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		rv = rv.Elem()
	}
	f := rv.FieldByName("SanitizeFingerprints")
	if !f.IsValid() {
		return errors.New("缺少 SanitizeFingerprints 字段")
	}
	if got := f.Type().String(); got != "atomic.Bool" {
		return fmt.Errorf("SanitizeFingerprints 类型是 %s，必须是 atomic.Bool（普通 bool 与面板热改并发读写是数据竞争）", got)
	}
	return nil
}

// 字段类型守卫：Client（New() 与结构体字面量两种构造路径）都必须是 atomic.Bool。
func TestSanitizeFingerprintsFieldIsAtomic(t *testing.T) {
	if err := sanitizeFlagTypeErr(New()); err != nil {
		t.Fatal(err)
	}
	if err := sanitizeFlagTypeErr(&Client{}); err != nil {
		t.Fatal(err)
	}
}

// 反事实验证：同一守卫对普通 bool 持有者必须报错（证明它不是恒真断言）。
func TestSanitizeFingerprintsAtomicGuardHasTeeth(t *testing.T) {
	type plainBoolHolder struct {
		SanitizeFingerprints bool
	}
	if err := sanitizeFlagTypeErr(&plainBoolHolder{}); err == nil {
		t.Fatal("守卫对普通 bool 字段无牙：字段退回 bool 时必须报错")
	}
	type atomicHolder struct {
		SanitizeFingerprints atomic.Bool
	}
	if err := sanitizeFlagTypeErr(&atomicHolder{}); err != nil {
		t.Fatalf("守卫误报 atomic.Bool: %v", err)
	}
}

// 零值语义：atomic.Bool 零值 false 与原 bool 零值一致（New() 显式 Store(true)）。
func TestSanitizeFingerprintsZeroValueMatchesBoolZero(t *testing.T) {
	if (&Client{}).SanitizeFingerprints.Load() {
		t.Fatal("结构体字面量（未 Store）应为 false，与原 bool 零值一致")
	}
	if !New().SanitizeFingerprints.Load() {
		t.Fatal("New() 默认必须为 true（与改动前 `SanitizeFingerprints: true` 等价）")
	}
}

// 热改立即生效：同一个 Client 上 Store 后**下一个请求**即改变出站 body。
func TestSanitizeFingerprintsHotReloadEffective(t *testing.T) {
	var gotBody []byte
	ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	defer ts.Close()

	c := New()
	c.ChatBaseCN = ts.URL
	acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"` + ccIdentity + `"}]}`)

	send := func() {
		t.Helper()
		rc, status, respBody, err := c.ChatStream(acct, body, "", ChatMeta{})
		if err != nil {
			t.Fatalf("chat stream: %v", err)
		}
		defer rc.Close()
		if status >= 400 {
			t.Fatalf("upstream status %d: %s", status, respBody)
		}
	}

	// 面板热改关闭脱敏 → 指纹原样出站。
	c.SanitizeFingerprints.Store(false)
	send()
	if !strings.Contains(string(gotBody), ccIdentity) {
		t.Fatalf("关闭时指纹应原样出站: %q", gotBody)
	}

	// 面板热改开启脱敏 → 同一 Client 的下一个请求就必须净化
	//（若热路径读的是构造期副本，这里会失败）。
	c.SanitizeFingerprints.Store(true)
	send()
	if strings.Contains(string(gotBody), ccIdentity) {
		t.Fatalf("热改开启后指纹仍出站（读到了陈旧副本）: %q", gotBody)
	}
}

// 并发压测：多 goroutine 同时 Store/Load，并同时压真实热路径 prepareBody 的 Load 点。
// 无 -race 时它只保证不崩；竞争消除的强证据是上面的类型守卫 + 热改生效断言。
func TestSanitizeFingerprintsConcurrentStoreLoad(t *testing.T) {
	c := New()
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 2000; j++ {
				c.SanitizeFingerprints.Store((i+j)%2 == 0)
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 2000; j++ {
				_ = c.SanitizeFingerprints.Load()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				_ = c.prepareBody(body, "cn", "u1", "c1")
			}
		}()
	}
	wg.Wait()
}
