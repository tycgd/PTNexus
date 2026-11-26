package handler

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// 图片代理参数：分片大小、总大小上限、单片/整体超时与重试。
const (
	imageProxyChunkSize  = 64 * 1024
	imageProxyMaxBytes   = 20 * 1024 * 1024
	imageProxyReqTimeout = 15 * time.Second
	imageProxyAllTimeout = 60 * time.Second
	imageProxyRetryTimes = 2
	imageProxyRetryDelay = 300 * time.Millisecond
)

// imageProxyAllowedDomains 为允许代理的图片站域名后缀白名单。
var imageProxyAllowedDomains = []string{
	"img.seedvault.cn",
	"pixhost.to",
	"pixhost.cc",
	"tu.totheglory.im",
}

// imageProxyFetcher 是实际抓图函数，抽成变量便于单测注入桩实现。
var imageProxyFetcher = fetchImageWithRange

// ImageProxyHandler 提供服务端图片代理，解决大图直连加载失败问题。
type ImageProxyHandler struct{}

// NewImageProxyHandler 创建图片代理 handler。
func NewImageProxyHandler() *ImageProxyHandler {
	return &ImageProxyHandler{}
}

// Proxy 处理 GET /api/image_proxy?u=<urlencoded 绝对图片URL>。
// 成功后回流图片字节；校验失败返回 400；上游抓取失败返回 502。
func (h *ImageProxyHandler) Proxy(c *gin.Context) {
	rawURL := strings.TrimSpace(c.Query("u"))
	if rawURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "缺少参数 u"})
		return
	}
	if err := validateImageProxyURL(rawURL); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}

	data, contentType, err := imageProxyFetcher(rawURL)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"success": false, "error": err.Error()})
		return
	}
	if strings.TrimSpace(contentType) == "" {
		contentType = "image/png"
	}
	c.Header("Content-Type", contentType)
	c.Header("Content-Length", strconv.Itoa(len(data)))
	c.Header("Cache-Control", "public, max-age=3600")
	c.Header("Accept-Ranges", "bytes")
	c.Status(http.StatusOK)
	_, _ = c.Writer.Write(data)
}

// validateImageProxyURL 校验目标 URL：必须为绝对 http/https、host 命中域名白名单、且非直连 IP/内网。
func validateImageProxyURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("非法 URL")
	}
	scheme := strings.ToLower(strings.TrimSpace(parsed.Scheme))
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("仅支持 http/https 协议")
	}
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(parsed.Hostname()), "."))
	if host == "" {
		return fmt.Errorf("非法 URL：缺少主机名")
	}
	if isBlockedImageProxyHost(host) {
		return fmt.Errorf("拒绝访问内网或直连 IP")
	}
	if !isAllowedImageProxyHost(host) {
		return fmt.Errorf("目标域名不在白名单内")
	}
	return nil
}

// isBlockedImageProxyHost 拒绝 localhost/环回/私网/链路本地地址（含所有直连 IP 字面量）。
func isBlockedImageProxyHost(host string) bool {
	switch host {
	case "localhost", "::1", "0.0.0.0":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		// 直连 IP 一律拒绝（含 127./10./192.168./172.16-31/169.254/::1 等内网与公网 IP）。
		return true
	}
	return false
}

// isAllowedImageProxyHost 判断 host 是否命中域名后缀白名单。
func isAllowedImageProxyHost(host string) bool {
	for _, domain := range imageProxyAllowedDomains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// fetchImageWithRange 用 Range 分片把图片取全，返回字节与上游 Content-Type。
// 首请求取 0-65535；若上游返回 206 则按 Content-Range 总大小循环 64KB 分片拼接（单片失败重试 2 次）；
// 若上游忽略 Range 返回 200，则直接读取整段（LimitReader 兜底）。总大小上限 20MB。
func fetchImageWithRange(rawURL string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), imageProxyAllTimeout)
	defer cancel()

	client := &http.Client{
		Timeout: imageProxyReqTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return fmt.Errorf("拒绝重定向")
		},
	}

	doGet := func(rangeHeader string) (*http.Response, error) {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if reqErr != nil {
			return nil, reqErr
		}
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; PTNexus/1.0)")
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		return client.Do(req)
	}

	resp, err := doGet(fmt.Sprintf("bytes=0-%d", imageProxyChunkSize-1))
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	contentType := resp.Header.Get("Content-Type")

	if resp.StatusCode == http.StatusOK {
		// 上游忽略 Range：直接读整段。
		body, readErr := readImageBody(resp.Body)
		if readErr != nil {
			return nil, "", readErr
		}
		return body, contentType, nil
	}
	if resp.StatusCode != http.StatusPartialContent {
		return nil, "", fmt.Errorf("上游返回非预期状态码: %d", resp.StatusCode)
	}

	total, parseErr := parseContentRangeTotal(resp.Header.Get("Content-Range"))
	if parseErr != nil {
		return nil, "", parseErr
	}
	if total > imageProxyMaxBytes {
		return nil, "", fmt.Errorf("图片过大（%d 字节，超过上限 %d）", total, imageProxyMaxBytes)
	}

	data, readErr := readImageBody(resp.Body)
	if readErr != nil {
		return nil, "", readErr
	}
	// 按「已累积的实际字节数」推进，避免上游短读分片时跳过空洞、静默拼出坏图。
	for len(data) < total {
		start := len(data)
		end := start + imageProxyChunkSize - 1
		if end >= total {
			end = total - 1
		}
		chunk, chunkErr := fetchImageChunkWithRetry(ctx, doGet, start, end)
		if chunkErr != nil {
			return nil, "", chunkErr
		}
		data = append(data, chunk...)
		if len(data) > imageProxyMaxBytes {
			return nil, "", fmt.Errorf("图片过大（超过上限 %d 字节）", imageProxyMaxBytes)
		}
	}
	return data, contentType, nil
}

// fetchImageChunkWithRetry 抓取单个分片，失败重试 imageProxyRetryTimes 次（间隔 300ms）。
func fetchImageChunkWithRetry(ctx context.Context, doGet func(string) (*http.Response, error), start, end int) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= imageProxyRetryTimes; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(imageProxyRetryDelay):
			}
		}
		resp, err := doGet(fmt.Sprintf("bytes=%d-%d", start, end))
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusPartialContent {
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("分片 %d-%d 返回状态码 %d", start, end, resp.StatusCode)
			continue
		}
		chunk, readErr := readImageBody(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if len(chunk) != end-start+1 {
			lastErr = fmt.Errorf("分片 %d-%d 短读：期望 %d 字节，实际 %d", start, end, end-start+1, len(chunk))
			continue
		}
		return chunk, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("分片 %d-%d 抓取失败", start, end)
	}
	return nil, lastErr
}

// readImageBody 读取响应体，超过 20MB 上限时报错。
func readImageBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, imageProxyMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败: %w", err)
	}
	if len(data) > imageProxyMaxBytes {
		return nil, fmt.Errorf("图片过大（超过上限 %d 字节）", imageProxyMaxBytes)
	}
	return data, nil
}

// parseContentRangeTotal 解析 Content-Range 的总大小（"bytes 0-65535/123456" → 123456）。
func parseContentRangeTotal(value string) (int, error) {
	value = strings.TrimSpace(value)
	idx := strings.LastIndex(value, "/")
	if idx < 0 || idx == len(value)-1 {
		return 0, fmt.Errorf("无法解析 Content-Range: %q", value)
	}
	totalStr := strings.TrimSpace(value[idx+1:])
	if totalStr == "*" {
		return 0, fmt.Errorf("上游未返回图片总大小")
	}
	total, err := strconv.Atoi(totalStr)
	if err != nil || total <= 0 {
		return 0, fmt.Errorf("无法解析 Content-Range 总大小: %q", value)
	}
	return total, nil
}
