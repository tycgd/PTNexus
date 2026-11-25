package repair

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withFastLskyTimings 缩短末日图床相关时序参数，避免测试真实等待；用例结束后还原。
func withFastLskyTimings(t *testing.T) {
	t.Helper()
	oldLoginBackoffs := lskyLoginBackoffs
	oldUploadBackoffs := lskyUploadBackoffs
	oldMinInterval := lskyLoginMinInterval
	oldTTL := lskyTokenTTL

	lskyLoginBackoffs = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond}
	lskyUploadBackoffs = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond}
	lskyLoginMinInterval = 15 * time.Second
	lskyTokenTTL = 24 * time.Hour

	t.Cleanup(func() {
		lskyLoginBackoffs = oldLoginBackoffs
		lskyUploadBackoffs = oldUploadBackoffs
		lskyLoginMinInterval = oldMinInterval
		lskyTokenTTL = oldTTL
	})
}

// resetLskyTokenState 清空 token 缓存，保证用例之间互不影响。
func resetLskyTokenState(t *testing.T) {
	t.Helper()
	lskyTokenManager.Lock()
	lskyTokenManager.entries = make(map[string]*lskyTokenEntry)
	lskyTokenManager.Unlock()
}

// writeTempImage 写入一个用于上传的临时图片文件。
func writeTempImage(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(path, []byte("fake-png-bytes"), 0o644); err != nil {
		t.Fatalf("写入临时图片失败: %v", err)
	}
	return path
}

// 用例 1：并发 10 个 goroutine 取 token，真实登录应只发生 1 次，其余全部复用缓存。
func TestGetLskyTokenReuseUnderConcurrency(t *testing.T) {
	withFastLskyTimings(t)
	resetLskyTokenState(t)

	var loginCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tokens" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&loginCount, 1)
		// 拉长登录窗口，验证其余 goroutine 在锁上等待后会命中缓存而不是再次登录。
		time.Sleep(20 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": true,
			"data":   map[string]any{"token": "tok-1"},
		})
	}))
	defer srv.Close()

	cfg := CheveretoUploadConfig{BaseURL: srv.URL, Email: "u@example.com", Password: "p"}
	const n = 10
	var wg sync.WaitGroup
	tokens := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			tokens[idx], errs[idx] = GetLskyToken(cfg)
		}(i)
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(&loginCount); got != 1 {
		t.Fatalf("并发 10 次取 token 应只真实登录 1 次，实际 %d 次", got)
	}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("第 %d 个 goroutine 取 token 失败: %v", i, errs[i])
		}
		if tokens[i] != "tok-1" {
			t.Fatalf("第 %d 个 goroutine token=%q, 期望 tok-1", i, tokens[i])
		}
	}
}

// 用例 2：登录前 2 次返回 429，第 3 次成功 → 断言最终成功且重试 2 次。
func TestLskyLoginBackoffRetriesOn429(t *testing.T) {
	withFastLskyTimings(t)
	resetLskyTokenState(t)

	var loginCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&loginCount, 1)
		if count <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("too many attempts"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": true,
			"data":   map[string]any{"token": "tok-ok"},
		})
	}))
	defer srv.Close()

	cfg := CheveretoUploadConfig{BaseURL: srv.URL, Email: "u@example.com", Password: "p"}
	token, err := GetLskyToken(cfg)
	if err != nil {
		t.Fatalf("429 退避后应成功，实际错误: %v", err)
	}
	if token != "tok-ok" {
		t.Fatalf("token=%q, 期望 tok-ok", token)
	}
	if got := atomic.LoadInt32(&loginCount); got != 3 {
		t.Fatalf("应重试 2 次共 3 次请求，实际 %d 次", got)
	}
}

// 用例 2（补充）：Retry-After 解析支持秒数与 HTTP 日期。
func TestParseRetryAfterDuration(t *testing.T) {
	secondsHeader := http.Header{}
	secondsHeader.Set("Retry-After", "30")
	if got := parseRetryAfterDuration(secondsHeader); got != 30*time.Second {
		t.Fatalf("秒数 Retry-After 解析=%v, 期望 30s", got)
	}

	dateHeader := http.Header{}
	dateHeader.Set("Retry-After", time.Now().Add(20*time.Second).UTC().Format(http.TimeFormat))
	if got := parseRetryAfterDuration(dateHeader); got <= 10*time.Second || got > 20*time.Second {
		t.Fatalf("HTTP 日期 Retry-After 解析=%v, 期望约 20s 以内", got)
	}

	if got := parseRetryAfterDuration(http.Header{}); got != 0 {
		t.Fatalf("空头应返回 0，实际 %v", got)
	}
}

// 用例 3：登录返回 401（凭据错误）应立即失败，不重试。
func TestLskyLoginDoesNotRetryOn401(t *testing.T) {
	withFastLskyTimings(t)
	resetLskyTokenState(t)

	var loginCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&loginCount, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("bad credentials"))
	}))
	defer srv.Close()

	cfg := CheveretoUploadConfig{BaseURL: srv.URL, Email: "u@example.com", Password: "wrong"}
	if _, err := GetLskyToken(cfg); err == nil {
		t.Fatal("401 应立即失败，不应返回 token")
	}
	if got := atomic.LoadInt32(&loginCount); got != 1 {
		t.Fatalf("401 不应重试，实际请求 %d 次", got)
	}
}

// 用例 4：缓存 token 失效后 15s 内再次登录 → 被登录间隔保护拒绝且不发请求。
func TestLskyLoginThrottleBlocksRepeatedLogin(t *testing.T) {
	withFastLskyTimings(t)
	resetLskyTokenState(t)
	lskyLoginMinInterval = time.Hour // 本用例内放大间隔，确保命中保护

	var loginCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&loginCount, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": true,
			"data":   map[string]any{"token": "tok-1"},
		})
	}))
	defer srv.Close()

	cfg := CheveretoUploadConfig{BaseURL: srv.URL, Email: "u@example.com", Password: "p"}
	if _, err := GetLskyToken(cfg); err != nil {
		t.Fatalf("首次登录应成功: %v", err)
	}

	// 模拟软 TTL 到期（obtainedAt 过期），但 lastLoginAt 仍很近。
	key := lskyTokenKey(cfg)
	lskyTokenManager.Lock()
	if entry := lskyTokenManager.entries[key]; entry != nil {
		entry.obtainedAt = time.Now().Add(-lskyTokenTTL - time.Hour)
	}
	lskyTokenManager.Unlock()

	_, err := GetLskyToken(cfg)
	if err == nil {
		t.Fatal("缓存失效后 15s 内再次登录应被登录间隔保护拒绝")
	}
	if !strings.Contains(err.Error(), "登录间隔保护") {
		t.Fatalf("期望命中登录间隔保护，实际错误: %v", err)
	}
	if got := atomic.LoadInt32(&loginCount); got != 1 {
		t.Fatalf("被节流时不应发起新的登录请求，实际请求 %d 次", got)
	}
}

// 用例 5a：上传首个请求返回 401 → 断言失效 token 并恰好 1 次重登后成功。
func TestUploadRetriesAfter401WithSingleRelogin(t *testing.T) {
	withFastLskyTimings(t)
	resetLskyTokenState(t)

	var loginCount, uploadCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/tokens":
			c := atomic.AddInt32(&loginCount, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": true,
				"data":   map[string]any{"token": fmt.Sprintf("tok-%d", c)},
			})
		case "/api/v1/upload":
			c := atomic.AddInt32(&uploadCount, 1)
			if c == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte("unauthorized"))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": true,
				"data": map[string]any{
					"links": map[string]any{"url": "https://img.seedvault.cn/a.png"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := CheveretoUploadConfig{BaseURL: srv.URL, Email: "u@example.com", Password: "p"}
	img := writeTempImage(t)
	logLine := func(format string, args ...any) {}
	url, err := UploadImageToCheveretoNarrative(img, cfg, "", logLine)
	if err != nil {
		t.Fatalf("401 重登后应上传成功，实际错误: %v", err)
	}
	if url != "https://img.seedvault.cn/a.png" {
		t.Fatalf("url=%q, 期望直链", url)
	}
	if got := atomic.LoadInt32(&uploadCount); got != 2 {
		t.Fatalf("上传请求应为 2 次（401 + 成功），实际 %d 次", got)
	}
	if got := atomic.LoadInt32(&loginCount); got != 2 {
		t.Fatalf("登录应为 2 次（初始 + 401 重登一次），实际 %d 次", got)
	}
}

// 用例 5b：上传持续 429 → 断言退避耗尽后报错（共 len(backoffs)+1 次尝试）。
func TestUploadBackoffExhaustedOnPersistent429(t *testing.T) {
	withFastLskyTimings(t)
	resetLskyTokenState(t)

	var uploadCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/upload" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&uploadCount, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("too many attempts"))
	}))
	defer srv.Close()

	cfg := CheveretoUploadConfig{BaseURL: srv.URL, Email: "u@example.com", Password: "p"}
	img := writeTempImage(t)
	if _, err := UploadImageToCheveretoNarrative(img, cfg, "tok-fixed", func(string, ...any) {}); err == nil {
		t.Fatal("持续 429 应在退避耗尽后返回错误")
	}
	expected := int32(len(lskyUploadBackoffs) + 1)
	if got := atomic.LoadInt32(&uploadCount); got != expected {
		t.Fatalf("上传尝试次数=%d, 期望 %d", got, expected)
	}
}

// 用例 6：失败策略分支。retry → Hoster 仍为 agsv、LoginErr 非空、UploadScreenshot 直接报错；
// fallback_pixhost → Hoster 显式回退为 pixhost（无凭据时登录失败不发起网络请求）。
func TestScreenshotUploadContextFailurePolicy(t *testing.T) {
	withFastLskyTimings(t)
	resetLskyTokenState(t)

	retryRoot := map[string]any{
		"cross_seed": map[string]any{
			"image_hoster": "agsv",
		},
	}
	ctx := PrepareScreenshotUploadContext(retryRoot)
	if ctx.Hoster != "agsv" {
		t.Fatalf("retry 策略下 Hoster 应保持 agsv，实际 %q", ctx.Hoster)
	}
	if ctx.FailurePolicy != "retry" {
		t.Fatalf("FailurePolicy=%q, 期望 retry", ctx.FailurePolicy)
	}
	if ctx.LoginErr == nil {
		t.Fatal("登录失败时 LoginErr 应非空")
	}
	if _, err := ctx.UploadScreenshot("not-exist.png", nil); err == nil {
		t.Fatal("retry 策略下登录失败，UploadScreenshot 应直接 fail-fast 报错")
	}

	fallbackRoot := map[string]any{
		"cross_seed": map[string]any{
			"image_hoster":                "agsv",
			"image_hoster_failure_policy": "fallback_pixhost",
		},
	}
	ctx2 := PrepareScreenshotUploadContext(fallbackRoot)
	if ctx2.FailurePolicy != "fallback_pixhost" {
		t.Fatalf("FailurePolicy=%q, 期望 fallback_pixhost", ctx2.FailurePolicy)
	}
	if ctx2.Hoster != "pixhost" {
		t.Fatalf("fallback_pixhost 策略下 Hoster 应回退 pixhost，实际 %q", ctx2.Hoster)
	}
}

// 用例 7：凭据缺失应直接报错，不发起网络请求。
func TestGetLskyTokenMissingCredentials(t *testing.T) {
	withFastLskyTimings(t)
	resetLskyTokenState(t)

	_, err := GetLskyToken(CheveretoUploadConfig{BaseURL: "https://img.seedvault.cn"})
	if err == nil {
		t.Fatal("凭据缺失应返回错误")
	}
	if !strings.Contains(err.Error(), "邮箱或密码未配置") {
		t.Fatalf("错误信息应提示凭据缺失，实际: %v", err)
	}
}