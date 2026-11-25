package repair

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pt-nexus/server/internal/platform/logx"
)

const lskyUploadLogModule = "迁移-末日图床上传"

// lskyDefaultDomain 是末日图床的固定域名。
const lskyDefaultDomain = "https://img.seedvault.cn"

// CheveretoUploadConfig 描述末日图床（Lsky Pro）上传所需的配置。
// 名称保留 CheveretoUploadConfig 以兼容已有调用方。
type CheveretoUploadConfig struct {
	BaseURL  string // 图床 API 根 URL，如 https://img.seedvault.cn（不含尾部斜杠）
	Email    string
	Password string
}

// lskyTokenResponse 描述 Lsky Pro 获取 token 的响应。
type lskyTokenResponse struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
	Data    struct {
		Token string `json:"token"`
	} `json:"data"`
}

// lskyUploadResponse 描述 Lsky Pro 上传响应。
type lskyUploadResponse struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
	Data    struct {
		Key        string  `json:"key"`
		Name       string  `json:"name"`
		OriginName string  `json:"origin_name"`
		Size       float64 `json:"size"`
		Mimetype   string  `json:"mimetype"`
		Extension  string  `json:"extension"`
		Links      struct {
			URL              string `json:"url"`
			HTML             string `json:"html"`
			BBCode           string `json:"bbcode"`
			Markdown         string `json:"markdown"`
			MarkdownWithLink string `json:"markdown_with_link"`
			ThumbnailURL     string `json:"thumbnail_url"`
		} `json:"links"`
	} `json:"data"`
}

// CheveretoLogin 使用邮箱和密码向 Lsky Pro 获取 Bearer token（单次请求）。
// 功能：兼容旧调用方的单次登录入口，内部委托给 cheveretoLoginOnce。
// 参数/返回：cfg 为图床配置；返回 token，失败返回错误。
// 失败场景：域名/凭据未配置、请求失败、HTTP 非 200、响应解析失败时返回错误。
// 副作用：会向末日图床发起一次外部 HTTP 请求；不做重试（重试由 lskyLoginWithBackoff 负责）。
// 函数名保留 CheveretoLogin 以兼容已有调用方。
func CheveretoLogin(cfg CheveretoUploadConfig) (string, error) {
	token, _, _, err := cheveretoLoginOnce(cfg)
	return token, err
}

// cheveretoLoginOnce 执行一次 Lsky Pro 登录请求。
// 功能：POST /api/v1/tokens 获取 token，并带回 HTTP 状态码与 Retry-After 建议等待时长。
// 参数/返回：cfg 为图床配置；返回 token、HTTP 状态码（网络错误为 0）、Retry-After 时长与错误。
// 失败场景：域名/凭据未配置、请求失败、HTTP 非 200、响应解析失败时返回错误。
// 副作用：会向末日图床发起一次外部 HTTP 请求；不打印密码与完整 token。
func cheveretoLoginOnce(cfg CheveretoUploadConfig) (string, int, time.Duration, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		return "", 0, 0, fmt.Errorf("末日图床域名未配置")
	}
	email := strings.TrimSpace(cfg.Email)
	password := strings.TrimSpace(cfg.Password)
	if email == "" || password == "" {
		return "", 0, 0, fmt.Errorf("末日图床邮箱或密码未配置")
	}

	tokenURL := baseURL + "/api/v1/tokens"
	logx.Infof(lskyUploadLogModule, "开始登录末日图床: %s email=%s", baseURL, email)

	payload, _ := json.Marshal(map[string]string{
		"email":    email,
		"password": password,
	})

	req, err := http.NewRequest(http.MethodPost, tokenURL, bytes.NewReader(payload))
	if err != nil {
		return "", 0, 0, fmt.Errorf("创建登录请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, 0, fmt.Errorf("登录请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	retryAfter := parseRetryAfterDuration(resp.Header)
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode, retryAfter, fmt.Errorf("末日图床登录失败 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tokenResp lskyTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", resp.StatusCode, retryAfter, fmt.Errorf("登录响应解析失败: %w body=%s", err, strings.TrimSpace(string(body)))
	}

	if !tokenResp.Status {
		return "", resp.StatusCode, retryAfter, fmt.Errorf("末日图床登录失败: %s", tokenResp.Message)
	}

	token := strings.TrimSpace(tokenResp.Data.Token)
	if token == "" {
		return "", resp.StatusCode, retryAfter, fmt.Errorf("末日图床未返回 token，响应: %s", strings.TrimSpace(string(body)))
	}

	logx.Infof(lskyUploadLogModule, "末日图床登录成功 email=%s", email)
	return token, resp.StatusCode, 0, nil
}

// UploadImageToChevereto 将本地图片上传到末日图床（Lsky Pro），返回图片直链 URL。
// 功能：单文件上传入口，内部含退避重试与 401 失效重登（token 为空时自动取缓存 token）。
// 参数/返回：imagePath 为本地图片路径；cfg 为图床配置；accessToken 为空时自动获取；返回直链。
// 失败场景：token 获取失败、文件打开/上传失败、退避重试耗尽、确定性 HTTP 错误（400/403/422）时返回错误。
// 副作用：会向末日图床发起上传请求；401 时会令缓存 token 失效并重登一次。
// 函数名保留 UploadImageToChevereto 以兼容已有调用方。
func UploadImageToChevereto(imagePath string, cfg CheveretoUploadConfig, accessToken string) (string, error) {
	return uploadImageToLskyWithRetry(imagePath, cfg, accessToken, nil)
}

// uploadImageToLskyWithRetry 执行带退避重试的末日图床上传。
// 功能：按 lskyUploadBackoffs 对 429/5xx/网络错误退避重试；401 时失效 token 并重登一次；400/403/422 立即失败。
// 参数/返回：imagePath 为本地图片；cfg 为图床配置；accessToken 为空时自动获取；logLine 为叙事日志回调（可为 nil）；返回直链。
// 失败场景：token 获取/重登失败、确定性 HTTP 错误、退避耗尽时返回错误。
// 副作用：会发起多次上传网络请求并按退避序列 sleep；可能触发一次 token 重登。
func uploadImageToLskyWithRetry(imagePath string, cfg CheveretoUploadConfig, accessToken string, logLine func(string, ...any)) (string, error) {
	if logLine == nil {
		logLine = func(string, ...any) {}
	}

	token := strings.TrimSpace(accessToken)
	if token == "" {
		newToken, err := GetLskyToken(cfg)
		if err != nil {
			logLine("❌ 获取末日图床 token 失败: %s", err)
			return "", err
		}
		token = newToken
	}

	backoffs := lskyUploadBackoffs
	relogged := false
	var lastErr error
	for attempt := 0; ; attempt++ {
		directURL, status, retryAfter, err := uploadImageToLskyOnce(imagePath, cfg, token, logLine)
		if err == nil {
			return directURL, nil
		}
		lastErr = err

		// 401：先做并发重登防抖——若缓存 token 已被其它 worker 刷新，直接用新 token 重试，不重复登录。
		if status == http.StatusUnauthorized && !relogged {
			relogged = true
			if cached := peekLskyToken(cfg); cached != "" && cached != token {
				logLine("⚠️ 上传返回 401，但缓存 token 已被其它任务刷新，改用新 token 重试...")
				token = cached
				continue
			}
			logLine("⚠️ 上传返回 401，token 失效，重新登录后重试...")
			invalidateLskyTokenIfMatch(cfg, token)
			newToken, loginErr := GetLskyToken(cfg)
			if loginErr != nil {
				logLine("❌ 重新登录失败: %s", loginErr)
				return "", loginErr
			}
			token = newToken
			continue
		}

		// 确定性错误：立即失败，不重试；已重登后再次 401 视为终态，快速失败避免空耗退避。
		if status == http.StatusBadRequest || status == http.StatusForbidden || status == http.StatusUnprocessableEntity || (status == http.StatusUnauthorized && relogged) {
			return "", lastErr
		}

		if attempt >= len(backoffs) {
			return "", lastErr
		}
		wait := backoffs[attempt]
		if retryAfter > wait {
			wait = retryAfter
		}
		if wait > lskyRetryAfterMaxWait {
			wait = lskyRetryAfterMaxWait
		}
		wait += lskyJitter(wait)
		logLine("⚠️ 上传失败（%s），%v 后第 %d 次重试...", lastErr, wait, attempt+1)
		logx.Warnf(lskyUploadLogModule, "末日图床上传失败 HTTP %d，%v 后第 %d 次重试 err=%v", status, wait, attempt+1, lastErr)
		time.Sleep(wait)
	}
}

// uploadImageToLskyOnce 执行一次末日图床上传请求（multipart 字段 file）。
// 功能：构建 multipart 请求并 POST /api/v1/upload，返回直链 URL、HTTP 状态码与 Retry-After 时长。
// 参数/返回：imagePath 为本地图片；cfg 为图床配置；token 为 Bearer token；logLine 为叙事日志回调；返回直链、状态码、Retry-After。
// 失败场景：域名/token 为空、文件打开失败、请求失败、HTTP 非 200、响应解析失败时返回错误。
// 副作用：会向末日图床发起一次外部上传请求；不打印完整 token。
func uploadImageToLskyOnce(imagePath string, cfg CheveretoUploadConfig, token string, logLine func(string, ...any)) (string, int, time.Duration, error) {
	if logLine == nil {
		logLine = func(string, ...any) {}
	}

	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		return "", 0, 0, fmt.Errorf("末日图床域名未配置")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", 0, 0, fmt.Errorf("末日图床 access token 为空")
	}

	file, err := os.Open(imagePath)
	if err != nil {
		logLine("❌ 打开文件失败: %s", err)
		return "", 0, 0, fmt.Errorf("打开文件失败 %s: %w", imagePath, err)
	}
	defer file.Close()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", filepath.Base(imagePath))
	if err != nil {
		logLine("❌ 创建表单失败: %s", err)
		return "", 0, 0, fmt.Errorf("创建表单文件失败: %w", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		logLine("❌ 写入文件数据失败: %s", err)
		return "", 0, 0, fmt.Errorf("写入文件数据失败: %w", err)
	}
	if err := writer.Close(); err != nil {
		logLine("❌ 关闭表单失败: %s", err)
		return "", 0, 0, fmt.Errorf("关闭 multipart writer 失败: %w", err)
	}

	uploadURL := baseURL + "/api/v1/upload"
	logx.Infof(lskyUploadLogModule, "上传文件到末日图床: %s file=%s", uploadURL, filepath.Base(imagePath))

	req, err := http.NewRequest(http.MethodPost, uploadURL, &buf)
	if err != nil {
		logLine("❌ 创建请求失败: %s", err)
		return "", 0, 0, fmt.Errorf("创建上传请求失败: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 180 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		logLine("❌ 上传请求失败: %s", classifyPixhostUploadError(err))
		return "", 0, 0, fmt.Errorf("上传请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	retryAfter := parseRetryAfterDuration(resp.Header)
	if resp.StatusCode != http.StatusOK {
		logLine("❌ 上传失败 HTTP %d: %s", resp.StatusCode, CompactLogText(strings.TrimSpace(string(body)), 240))
		return "", resp.StatusCode, retryAfter, fmt.Errorf("末日图床上传失败 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var uploadResp lskyUploadResponse
	if err := json.Unmarshal(body, &uploadResp); err != nil {
		logLine("❌ 响应解析失败: %s", err)
		return "", resp.StatusCode, retryAfter, fmt.Errorf("上传响应解析失败: %w body=%s", err, strings.TrimSpace(string(body)))
	}

	if !uploadResp.Status {
		logLine("❌ 上传失败: %s", uploadResp.Message)
		return "", resp.StatusCode, retryAfter, fmt.Errorf("末日图床上传失败: %s", uploadResp.Message)
	}

	directURL := strings.TrimSpace(uploadResp.Data.Links.URL)
	if directURL == "" {
		logLine("❌ 未返回图片 URL")
		return "", resp.StatusCode, retryAfter, fmt.Errorf("末日图床未返回图片 URL，响应: %s", strings.TrimSpace(string(body)))
	}

	logx.Infof(lskyUploadLogModule, "上传成功: %s → %s", filepath.Base(imagePath), directURL)
	return directURL, resp.StatusCode, 0, nil
}

// UploadImageToCheveretoNarrative 按叙事式日志风格上传图片到末日图床，与 UploadImageToPixhostNarrativeWithLogger 风格一致。
// 功能：叙事式日志上传入口，内部复用带退避重试与 401 重登的上传逻辑。
// 参数/返回：imagePath 为本地图片；cfg 为图床配置；accessToken 为空时自动获取；logLine 为叙事日志回调；返回直链。
// 失败场景：文件不存在、域名未配置、token 获取/重登失败、退避耗尽时返回错误。
// 副作用：会向末日图床发起上传请求并输出叙事式日志；401 时会令缓存 token 失效并重登一次。
func UploadImageToCheveretoNarrative(imagePath string, cfg CheveretoUploadConfig, accessToken string, logLine func(string, ...any)) (string, error) {
	if logLine == nil {
		logLine = logx.PlainInfof
	}

	logLine("准备上传到末日图床: %s", imagePath)
	if _, err := os.Stat(imagePath); err != nil {
		logLine("错误：文件不存在 %s", imagePath)
		return "", err
	}

	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		logLine("错误：末日图床域名未配置")
		return "", fmt.Errorf("末日图床域名未配置")
	}

	directURL, err := uploadImageToLskyWithRetry(imagePath, cfg, accessToken, logLine)
	if err != nil {
		return "", err
	}

	logLine("直接上传成功！图片链接: %s", directURL)
	return directURL, nil
}

// GetImageHosterFromConfig 从 rootConfig 的 cross_seed 节提取 image_hoster 值。
func GetImageHosterFromConfig(rootConfig map[string]any) string {
	if rootConfig == nil {
		return "pixhost"
	}
	cs, ok := rootConfig["cross_seed"].(map[string]any)
	if !ok {
		return "pixhost"
	}
	hoster := strings.TrimSpace(toStringAny(cs["image_hoster"], ""))
	if hoster == "" {
		return "pixhost"
	}
	return hoster
}

// GetCheveretoConfigFromRootConfig 从 rootConfig 的 cross_seed 节提取末日图床上传配置。
// 域名固定为 https://img.seedvault.cn，无需用户配置。
func GetCheveretoConfigFromRootConfig(rootConfig map[string]any) CheveretoUploadConfig {
	cfg := CheveretoUploadConfig{
		BaseURL: lskyDefaultDomain,
	}
	if rootConfig == nil {
		return cfg
	}
	cs, ok := rootConfig["cross_seed"].(map[string]any)
	if !ok {
		return cfg
	}
	cfg.Email = strings.TrimSpace(toStringAny(cs["agsv_email"], ""))
	cfg.Password = strings.TrimSpace(toStringAny(cs["agsv_password"], ""))
	return cfg
}

// TransferRemoteImageToChevereto 将远程图片下载后上传到末日图床，返回图片直链。
func TransferRemoteImageToChevereto(imageURL string, cfg CheveretoUploadConfig, accessToken string) (string, error) {
	trimmed := strings.TrimSpace(imageURL)
	if trimmed == "" {
		return "", fmt.Errorf("empty image url")
	}

	candidates := buildPosterDownloadCandidates(trimmed)
	logx.Infof(lskyUploadLogModule, "开始海报转存到末日图床 source=%s candidates=%d", CompactLogText(trimmed, 160), len(candidates))

	errMsgs := make([]string, 0, len(candidates)*posterTransferDownloadRetry)

	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		for attempt := 1; attempt <= posterTransferDownloadRetry; attempt++ {
			data, contentType, downloadErr := downloadPosterImage(candidate)
			if downloadErr != nil {
				errMsgs = append(errMsgs, fmt.Sprintf("下载失败 candidate=%s attempt=%d err=%v", CompactLogText(candidate, 120), attempt, downloadErr))
				logx.Warnf(lskyUploadLogModule, "下载海报失败 candidate=%s attempt=%d/%d err=%v", CompactLogText(candidate, 160), attempt, posterTransferDownloadRetry, downloadErr)
				if attempt < posterTransferDownloadRetry {
					time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)
				}
				continue
			}

			logx.Infof(
				lskyUploadLogModule,
				"下载海报成功 candidate=%s attempt=%d bytes=%d content_type=%s",
				CompactLogText(candidate, 160), attempt, len(data), contentType,
			)

			tmpPath, tmpErr := writePosterTempFile(data, contentType)
			if tmpErr != nil {
				errMsgs = append(errMsgs, fmt.Sprintf("落盘失败 candidate=%s err=%v", CompactLogText(candidate, 120), tmpErr))
				logx.Warnf(lskyUploadLogModule, "海报临时文件写入失败 candidate=%s err=%v", CompactLogText(candidate, 160), tmpErr)
				continue
			}

			directURL, uploadErr := UploadImageToChevereto(tmpPath, cfg, accessToken)
			_ = os.Remove(tmpPath)
			if uploadErr != nil {
				errMsgs = append(errMsgs, fmt.Sprintf("上传失败 candidate=%s attempt=%d err=%v", CompactLogText(candidate, 120), attempt, uploadErr))
				logx.Warnf(lskyUploadLogModule, "上传海报到末日图床失败 candidate=%s attempt=%d/%d err=%v", CompactLogText(candidate, 160), attempt, posterTransferDownloadRetry, uploadErr)
				if attempt < posterTransferDownloadRetry {
					time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)
				}
				continue
			}

			logx.Infof(lskyUploadLogModule, "海报转存末日图床成功 source=%s target=%s", CompactLogText(trimmed, 160), CompactLogText(directURL, 160))
			return strings.TrimSpace(directURL), nil
		}
	}

	if len(errMsgs) == 0 {
		return "", fmt.Errorf("poster transfer to lsky failed without details")
	}
	return "", fmt.Errorf(strings.Join(errMsgs, " | "))
}

// GetImageHosterFailurePolicyFromConfig 从 rootConfig 的 cross_seed 节提取图床上传失败策略。
// 功能：读取 cross_seed.image_hoster_failure_policy，仅接受 retry / fallback_pixhost。
// 参数/返回：rootConfig 为应用根配置；返回 "retry"（默认）或 "fallback_pixhost"。
// 失败场景：配置缺失或非法值时回退为 "retry"。
// 副作用：仅读取内存中的配置，不写配置、不发请求。
func GetImageHosterFailurePolicyFromConfig(rootConfig map[string]any) string {
	if rootConfig == nil {
		return "retry"
	}
	cs, ok := rootConfig["cross_seed"].(map[string]any)
	if !ok {
		return "retry"
	}
	policy := strings.TrimSpace(toStringAny(cs["image_hoster_failure_policy"], ""))
	if policy == "fallback_pixhost" {
		return policy
	}
	return "retry"
}

// ScreenshotUploadContext 封装截图上传所需的上下文。
type ScreenshotUploadContext struct {
	Hoster        string
	Chevereto     CheveretoUploadConfig
	Pixhost       PixhostUploadConfig
	AccessToken   string
	FailurePolicy string // retry | fallback_pixhost（来自 cross_seed.image_hoster_failure_policy，默认 retry）
	LoginErr      error  // 登录失败且策略为 retry 时记录，用于 fail-fast
}

// PrepareScreenshotUploadContext 从 rootConfig 构建截图上传上下文，按 image_hoster 配置分派。
// 功能：读取图床与失败策略；末日图床场景下通过 token 管理器（含缓存/退避）获取 token。
// 参数/返回：rootConfig 为应用根配置；返回截图上传上下文（含 FailurePolicy 与可能的 LoginErr）。
// 失败场景：末日图床登录失败时，retry 策略记录 LoginErr 但不回退；fallback_pixhost 策略显式回退 Pixhost。
// 副作用：末日图床场景下可能发起登录网络请求（缓存命中则不发起）。
func PrepareScreenshotUploadContext(rootConfig map[string]any) ScreenshotUploadContext {
	hoster := GetImageHosterFromConfig(rootConfig)
	policy := GetImageHosterFailurePolicyFromConfig(rootConfig)
	ctx := ScreenshotUploadContext{
		Hoster:        hoster,
		Pixhost:       GetPixhostUploadConfigFromRootConfig(rootConfig),
		FailurePolicy: policy,
	}
	if hoster == "agsv" {
		cfg := GetCheveretoConfigFromRootConfig(rootConfig)
		ctx.Chevereto = cfg
		token, err := GetLskyToken(cfg)
		if err != nil {
			ctx.LoginErr = err
			if policy == "fallback_pixhost" {
				logx.Errorf(lskyUploadLogModule, "末日图床登录失败（已重试），⚠️ 按配置回退 Pixhost: %v", err)
				ctx.Hoster = "pixhost"
			} else {
				logx.Errorf(lskyUploadLogModule, "末日图床登录失败（已重试），本次任务将中止: %v", err)
			}
			return ctx
		}
		ctx.AccessToken = token
		logx.Infof(lskyUploadLogModule, "末日图床登录成功，准备上传截图")
	}
	return ctx
}

// UploadScreenshot 执行单张截图上传，按 hoster 类型分派。
// 功能：末日图床下若登录已失败（LoginErr 非空）直接 fail-fast，避免逐图重登；否则走带退避上传。
// 参数/返回：filePath 为本地图片；logLine 为叙事日志回调；返回上传后的 URL（末日图床为直链）。
// 失败场景：登录失败、上传退避耗尽时返回错误。
// 副作用：会向图床发起上传请求。
func (ctx ScreenshotUploadContext) UploadScreenshot(filePath string, logLine func(string, ...any)) (string, error) {
	if ctx.Hoster == "agsv" {
		if ctx.LoginErr != nil {
			return "", ctx.LoginErr
		}
		return UploadImageToCheveretoNarrative(filePath, ctx.Chevereto, ctx.AccessToken, logLine)
	}
	return UploadImageToPixhostNarrativeWithConfigAndLogger(filePath, ctx.Pixhost, logLine)
}

// NormalizeScreenshotURL 将上传返回的 URL 规范化。Pixhost 需要将 show_url 转为直链，末日图床直接返回直链。
func (ctx ScreenshotUploadContext) NormalizeScreenshotURL(showURL string) string {
	if ctx.Hoster == "agsv" {
		return strings.TrimSpace(showURL)
	}
	finalURL := strings.TrimSpace(showURL)
	if direct := PixhostShowToDirectURLWithConfig(showURL, ctx.Pixhost); strings.TrimSpace(direct) != "" {
		if normalized := NormalizePixhostDirectHostWithConfig(direct, ctx.Pixhost); strings.TrimSpace(normalized) != "" {
			finalURL = normalized
		} else {
			finalURL = direct
		}
	}
	return finalURL
}
