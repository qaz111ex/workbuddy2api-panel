package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// startTestServer 起一个真实监听器 + http.Server，返回监听地址与 srv。
func startTestServer(t *testing.T, h http.Handler) (net.Listener, *http.Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	return ln, srv
}

// TestServeUntilShutdownWaitsForInFlightRequest 锁定真实缺陷：srv.Serve 在 Shutdown
// **一开始**就返回 http.ErrServerClosed，若调用方随即返回，进程会在在途请求还没写完
// 时就退出——正在生成的 SSE 流被掐断。本用例断言 serveUntilShutdown 必须等到在途请求
// 自然结束才返回。
func TestServeUntilShutdownWaitsForInFlightRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release // 模拟长流式生成：请求在途，连接不空闲
		_, _ = io.WriteString(w, "stream-done")
	})

	ln, srv := startTestServer(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var returned atomic.Bool
	var serveErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveErr = serveUntilShutdown(srv, ln, ctx, 5*time.Second, nil)
		returned.Store(true)
	}()

	// 发起一个在途请求并确认已进入 handler。
	respCh := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			respCh <- "ERR:" + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		respCh <- string(b)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("请求未能进入 handler")
	}

	// 触发停机（等价 SIGTERM）。
	cancel()

	// 关键断言：在途请求未结束时，serveUntilShutdown **不得**返回。
	select {
	case <-done:
		t.Fatalf("serveUntilShutdown 在在途请求结束前就返回了（优雅停机等于没做）：serveErr=%v returned=%v",
			serveErr, returned.Load())
	case <-time.After(300 * time.Millisecond):
		// 正确：仍在等待排空
	}

	// 放行在途请求：此刻必须能正常写完响应并被客户端完整收到。
	close(release)

	select {
	case body := <-respCh:
		if body != "stream-done" {
			t.Fatalf("在途响应被截断: got %q want %q", body, "stream-done")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("在途请求未在宽限期内完成")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("排空完成后 serveUntilShutdown 仍未返回")
	}
	if serveErr != nil {
		t.Fatalf("serveUntilShutdown 返回错误: %v", serveErr)
	}
}

// TestServeUntilShutdownCallsOnSignalBeforeDrain 断言 onSignal（生产里是状态落盘）
// 在排空过程中被调用，且语义是「先持久化再停机」。
func TestServeUntilShutdownCallsOnSignalBeforeDrain(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "ok")
	})

	ln, srv := startTestServer(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	signalCalled := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = serveUntilShutdown(srv, ln, ctx, 5*time.Second, func() { close(signalCalled) })
	}()

	go func() { _, _ = http.Get("http://" + ln.Addr().String() + "/") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("请求未能进入 handler")
	}

	cancel()
	select {
	case <-signalCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("onSignal 未被调用（状态落盘被跳过）")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serveUntilShutdown 未返回")
	}
}

// TestServeUntilShutdownGraceBoundsWait 断言宽限期是**上限**：在途请求永不结束时，
// serveUntilShutdown 仍在 grace 后返回（不会把进程永久挂住）。
func TestServeUntilShutdownGraceBoundsWait(t *testing.T) {
	entered := make(chan struct{})
	block := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-block // 永不结束
	})

	ln, srv := startTestServer(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer close(block)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = serveUntilShutdown(srv, ln, ctx, 150*time.Millisecond, nil)
	}()

	go func() { _, _ = http.Get("http://" + ln.Addr().String() + "/") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("请求未能进入 handler")
	}

	start := time.Now()
	cancel()
	select {
	case <-done:
		// 必须受 grace 限时，而不是被永不结束的请求挂死。
		if el := time.Since(start); el > 3*time.Second {
			t.Fatalf("宽限期未生效，等待过久: %v", el)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("在途请求永不结束时应由 grace 限时返回，实际被挂死")
	}
}

// TestConfigShutdownGraceDefault 锁定配置回落：键缺席/为 0/负数一律回落 5s。
func TestConfigShutdownGraceDefault(t *testing.T) {
	for _, in := range []int{0, -1, -3600} {
		c := Default()
		c.ShutdownGraceSeconds = in
		if err := c.normalize(); err != nil {
			t.Fatalf("normalize(%d): %v", in, err)
		}
		if c.ShutdownGraceSeconds != 5 {
			t.Errorf("ShutdownGraceSeconds=%d normalize 后 = %d, want 5", in, c.ShutdownGraceSeconds)
		}
	}
	// 显式正数必须原样保留（用户调大宽限期是受支持用法）。
	c := Default()
	c.ShutdownGraceSeconds = 120
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.ShutdownGraceSeconds != 120 {
		t.Errorf("显式 120 被改写为 %d", c.ShutdownGraceSeconds)
	}
}
