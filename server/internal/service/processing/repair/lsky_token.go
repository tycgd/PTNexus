package repair

import (
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pt-nexus/server/internal/platform/logx"
)

// 说明：本文件负责末日图床（Lsky Pro）token 的进程内复用与登录退避。
// 目的：避免每个截图任务/每张海报都重新登录，导致上游 HTTP 429 Too Many Attempts。

// lskyTokenEntry 缓存单个账号（BaseURL + Email）的 token 状态。
type lskyTokenEntry struct {
	token       string    // 最近一次成功登录取得的 token
	obtainedAt  time.Time // token 取得时间（用于软 TTL 判断）
	lastLoginAt time.Time // 最近一次真实登录尝试时间（用于登录最小间隔保护）
}

// lskyTokenManager 是包级单例 token 管理器，进程内全局共享（跨任务/跨批量复用）。
// 以 BaseURL + "\n" + Email 作为键区分不同图床账号。
var lskyTokenManager = struct {
	sync.Mutex
	entries map[string]*lskyTokenEntry
}{entries: make(map[string]*lskyTokenEntry)}

// 以下是可被测试覆盖的时序参数（生产默认值见注释）。
var (
	// lskyTokenTTL 软 TTL：到期后下一次取用时重新登录（Lsky token 实际长期有效，双保险）。
	lskyTokenTTL = 24 * time.Hour
	// lskyLoginMinInterval 同账号两次真实登录的最小间隔，用于抵御登录风暴。
	lskyLoginMinInterval = 15 * time.Second
	// lskyLoginBackoffs 登录失败（429/5xx/网络错误）时的退避序列，共重试 len 次。
	lskyLoginBackoffs = []time.Duration{5 * time.Second, 10 * time.Second}
	// lskyUploadBackoffs 上传失败（429/5xx/网络错误）时的退避序列，共重试 len 次。
	lskyUploadBackoffs = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}
	// lskyRetryAfterMaxWait 单次退避等待的硬上限，防止上游返回超大 Retry-After 时长冻结链路。
	lskyRetryAfterMaxWait = 2 * time.Minute
)

// lskyTokenKey 生成 token 缓存键（BaseURL 去掉尾部斜杠 + 换行 + 邮箱）。
func lskyTokenKey(cfg CheveretoUploadConfig) string {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	return baseURL + "\n" + strings.TrimSpace(cfg.Email)
}

// GetLskyToken 获取末日图床 token，优先命中进程内缓存。
// 功能：按 (BaseURL, Email) 复用 token，未命中或已过期时执行带退避的登录。
// 参数/返回：cfg 为图床配置；返回可用 token，失败返回错误。
// 失败场景：域名/凭据未配置、登录间隔保护触发、登录请求退避重试耗尽时返回错误。
// 副作用：缓存未命中且不在登录间隔内时，会发起真实登录网络请求；登录过程串行（全局 Mutex 单飞）。
func GetLskyToken(cfg CheveretoUploadConfig) (string, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		return "", fmt.Errorf("末日图床域名未配置")
	}
	if strings.TrimSpace(cfg.Email) == "" || strings.TrimSpace(cfg.Password) == "" {
		return "", fmt.Errorf("末日图床邮箱或密码未配置")
	}

	key := lskyTokenKey(cfg)

	lskyTokenManager.Lock()
	defer lskyTokenManager.Unlock()

	entry := lskyTokenManager.entries[key]
	if entry == nil {
		entry = &lskyTokenEntry{}
		lskyTokenManager.entries[key] = entry
	}

	// ① 命中缓存：并发上传全部走这里，零网络开销。
	if entry.token != "" && time.Since(entry.obtainedAt) < lskyTokenTTL {
		return entry.token, nil
	}

	// ② 登录间隔保护：间隔内的重复登录请求直接拒绝，避免登录风暴。
	if !entry.lastLoginAt.IsZero() && time.Since(entry.lastLoginAt) < lskyLoginMinInterval {
		return "", fmt.Errorf("末日图床登录间隔保护（%v 内不重复登录）", lskyLoginMinInterval)
	}

	// ③ 真实登录（含退避重试）：登录在锁内执行，天然串行。
	entry.lastLoginAt = time.Now()
	token, err := lskyLoginWithBackoff(cfg)
	if err != nil {
		return "", err
	}
	entry.token = token
	entry.obtainedAt = time.Now()
	return token, nil
}

// InvalidateLskyToken 令指定账号的缓存 token 失效（上传收到 401 时调用）。
// 功能：删除缓存条目，使下一次 GetLskyToken 重新登录。
// 参数/返回：cfg 为图床配置；无返回值。
// 失败场景：无。
// 副作用：仅修改进程内缓存，不发起网络请求。
func InvalidateLskyToken(cfg CheveretoUploadConfig) {
	key := lskyTokenKey(cfg)
	lskyTokenManager.Lock()
	defer lskyTokenManager.Unlock()
	if lskyTokenManager.entries != nil {
		delete(lskyTokenManager.entries, key)
	}
}

// peekLskyToken 只读取当前缓存中的有效 token，不触发任何登录。
// 功能：供 401 并发重登防抖判断「别的 worker 是否已刷新出新 token」。
// 参数/返回：cfg 为图床配置；返回缓存中的有效 token，无有效缓存时返回空字符串。
// 失败场景：无（未命中或已过期按空字符串处理）。
// 副作用：仅读取进程内缓存，不发起网络请求。
func peekLskyToken(cfg CheveretoUploadConfig) string {
	key := lskyTokenKey(cfg)
	lskyTokenManager.Lock()
	defer lskyTokenManager.Unlock()
	entry := lskyTokenManager.entries[key]
	if entry == nil || entry.token == "" || time.Since(entry.obtainedAt) >= lskyTokenTTL {
		return ""
	}
	return entry.token
}

// invalidateLskyTokenIfMatch 仅在缓存 token 与 expected 一致时删除条目（401 并发重登防抖）。
// 功能：避免误删其它 worker 已刷新写回的新 token，导致重复登录风暴。
// 参数/返回：cfg 为图床配置；expected 为本次失败请求所用的 token；返回是否实际删除了条目。
// 失败场景：无。
// 副作用：可能删除进程内缓存条目，不发起网络请求。
func invalidateLskyTokenIfMatch(cfg CheveretoUploadConfig, expected string) bool {
	key := lskyTokenKey(cfg)
	expected = strings.TrimSpace(expected)
	lskyTokenManager.Lock()
	defer lskyTokenManager.Unlock()
	entry := lskyTokenManager.entries[key]
	if entry == nil {
		return false
	}
	if expected != "" && strings.TrimSpace(entry.token) != expected {
		return false
	}
	delete(lskyTokenManager.entries, key)
	return true
}

// lskyLoginWithBackoff 带退避地执行末日图床登录（包装 cheveretoLoginOnce 单次请求）。
// 功能：登录失败时按 lskyLoginBackoffs 退避重试，尊重 Retry-After。
// 参数/返回：cfg 为图床配置；返回 token，失败返回错误。
// 失败场景：凭据错误（400/401/422）立即失败；其余错误重试耗尽后返回最后错误。
// 副作用：会发起多次登录网络请求并按退避序列 sleep；不打印密码与完整 token。
func lskyLoginWithBackoff(cfg CheveretoUploadConfig) (string, error) {
	backoffs := lskyLoginBackoffs
	for attempt := 0; ; attempt++ {
		token, status, retryAfter, err := cheveretoLoginOnce(cfg)
		if err == nil {
			return token, nil
		}
		// 凭据类确定性错误：立即失败，不重试。
		if status == http.StatusBadRequest || status == http.StatusUnauthorized || status == http.StatusUnprocessableEntity {
			return "", err
		}
		if attempt >= len(backoffs) {
			return "", err
		}
		wait := backoffs[attempt]
		if retryAfter > wait {
			wait = retryAfter
		}
		if wait > lskyRetryAfterMaxWait {
			wait = lskyRetryAfterMaxWait
		}
		wait += lskyJitter(wait)
		logx.Warnf(lskyUploadLogModule, "末日图床登录失败 HTTP %d，%v 后第 %d 次重试", status, wait, attempt+1)
		time.Sleep(wait)
	}
}

// lskyJitter 返回 [0, d*20%] 的随机抖动，避免多任务同时重试造成新的尖峰。
func lskyJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	maxJitter := int64(d) / 5 // 20%
	if maxJitter <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(maxJitter))
}

// parseRetryAfterDuration 解析 Retry-After 响应头（支持秒数与 HTTP 日期两种格式）。
// 功能：把上游建议的等待时长转换为 time.Duration。
// 参数/返回：header 为响应头；返回建议等待时长，缺失或无法解析时返回 0。
// 失败场景：无（解析失败按 0 处理）。
// 副作用：无。
func parseRetryAfterDuration(header http.Header) time.Duration {
	if header == nil {
		return 0
	}
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if when, err := http.ParseTime(raw); err == nil {
		if wait := time.Until(when); wait > 0 {
			return wait
		}
	}
	return 0
}
