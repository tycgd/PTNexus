package handler

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// newRangeTestServer 构造支持 Range 的测试图片服务，用于验证 206 分片拼接。
func newRangeTestServer(t *testing.T, data []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		var start, end int
		if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if start < 0 || start >= len(data) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if end >= len(data) {
			end = len(data) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
}

// 用例：某 206 分片实际返回长度短于声明范围时，应被识别为失败并最终报错（不静默拼坏图）。
func TestFetchImageWithRangeShortChunkFails(t *testing.T) {
	payload := bytes.Repeat([]byte("z"), imageProxyChunkSize*2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if end >= len(payload) {
			end = len(payload) - 1
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		if start == 0 {
			_, _ = w.Write(payload[start : end+1])
			return
		}
		// 非首片故意只写一半，模拟上游短读。
		half := (end - start + 1) / 2
		_, _ = w.Write(payload[start : start+half])
	}))
	defer server.Close()

	if _, _, err := fetchImageWithRange(server.URL + "/short.png"); err == nil {
		t.Fatal("短读分片应被识别为失败并返回错误")
	}
}

// 用例：206 + Content-Range，跨 3 个分片也能拼出完整图片。
func TestFetchImageWithRangePartialContent(t *testing.T) {
	raw := bytes.Repeat([]byte("abcdefghij"), (imageProxyChunkSize*2+100)/10+1)
	payload := raw[:imageProxyChunkSize*2+100]
	server := newRangeTestServer(t, payload)
	defer server.Close()

	data, contentType, err := fetchImageWithRange(server.URL + "/a.png")
	if err != nil {
		t.Fatalf("fetchImageWithRange 失败: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("拼接结果长度=%d，期望 %d", len(data), len(payload))
	}
	if contentType != "image/png" {
		t.Fatalf("Content-Type=%q, 期望 image/png", contentType)
	}
}

// 用例：上游忽略 Range 直接返回 200 整段，也能取全。
func TestFetchImageWithRangeIgnoresRange(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 5000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	data, contentType, err := fetchImageWithRange(server.URL + "/b.jpg")
	if err != nil {
		t.Fatalf("fetchImageWithRange 失败: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("整段读取结果长度=%d，期望 %d", len(data), len(payload))
	}
	if contentType != "image/jpeg" {
		t.Fatalf("Content-Type=%q, 期望 image/jpeg", contentType)
	}
}

// 用例：上游重定向时 CheckRedirect 拒绝，fetch 返回错误。
func TestFetchImageWithRangeRejectsRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("final"))
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/final.png", http.StatusFound)
	}))
	defer redirector.Close()

	if _, _, err := fetchImageWithRange(redirector.URL + "/start.png"); err == nil {
		t.Fatal("重定向应被拒绝并返回错误")
	}
}

// newImageProxyTestRouter 构造仅含图片代理路由的 gin 测试引擎。
func newImageProxyTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	handler := NewImageProxyHandler()
	engine.GET("/api/image_proxy", handler.Proxy)
	return engine
}

func doImageProxyRequest(engine *gin.Engine, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	return recorder
}

// 用例：参数缺失/非 http/https/内网 IP/白名单外 host 均返回 400。
func TestImageProxyBadRequests(t *testing.T) {
	engine := newImageProxyTestRouter()
	cases := []string{
		"/api/image_proxy",
		"/api/image_proxy?u=notaurl",
		"/api/image_proxy?u=ftp%3A%2F%2Fimg.seedvault.cn%2Fa.png",
		"/api/image_proxy?u=http%3A%2F%2F127.0.0.1%2Fa.png",
		"/api/image_proxy?u=http%3A%2F%2F192.168.1.2%2Fa.png",
		"/api/image_proxy?u=http%3A%2F%2Flocalhost%2Fa.png",
		"/api/image_proxy?u=http%3A%2F%2Fevil.example.com%2Fa.png",
		"/api/image_proxy?u=http%3A%2F%2Fevilimg.seedvault.cn.attacker.com%2Fa.png",
	}
	for _, target := range cases {
		recorder := doImageProxyRequest(engine, target)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s 期望 400，实际 %d（body=%s）", target, recorder.Code, recorder.Body.String())
		}
	}
}

// 用例：上游抓取失败返回 502 JSON。
func TestImageProxyUpstreamFailure(t *testing.T) {
	old := imageProxyFetcher
	imageProxyFetcher = func(string) ([]byte, string, error) {
		return nil, "", fmt.Errorf("模拟上游失败")
	}
	t.Cleanup(func() { imageProxyFetcher = old })

	engine := newImageProxyTestRouter()
	recorder := doImageProxyRequest(engine, "/api/image_proxy?u=https%3A%2F%2Fimg.seedvault.cn%2Fa.png")
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("期望 502，实际 %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"success":false`) {
		t.Fatalf("502 响应应为 JSON success=false，实际 %s", recorder.Body.String())
	}
}

// 用例：命中白名单且上游成功时，回传字节与缓存/长度响应头。
func TestImageProxySuccessResponse(t *testing.T) {
	old := imageProxyFetcher
	imageProxyFetcher = func(string) ([]byte, string, error) {
		return []byte("image-bytes"), "image/webp", nil
	}
	t.Cleanup(func() { imageProxyFetcher = old })

	engine := newImageProxyTestRouter()
	recorder := doImageProxyRequest(engine, "/api/image_proxy?u=https%3A%2F%2Fimg.seedvault.cn%2Fa.png")
	if recorder.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", recorder.Code)
	}
	if recorder.Body.String() != "image-bytes" {
		t.Fatalf("响应体=%q", recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "image/webp" {
		t.Fatalf("Content-Type=%q", got)
	}
	if got := recorder.Header().Get("Content-Length"); got != "11" {
		t.Fatalf("Content-Length=%q，期望 11", got)
	}
	if got := recorder.Header().Get("Cache-Control"); !strings.Contains(got, "max-age=3600") {
		t.Fatalf("Cache-Control=%q", got)
	}
}

// 用例：白名单后缀匹配（子域命中、相似域名不命中）。
func TestValidateImageProxyURL(t *testing.T) {
	allowed := []string{
		"https://img.seedvault.cn/a/b.png",
		"https://t1.pixhost.to/thumbs/x.jpg",
		"https://img2.pixhost.cc/images/x.jpg",
		"https://tu.totheglory.im/x.jpg",
	}
	for _, raw := range allowed {
		if err := validateImageProxyURL(raw); err != nil {
			t.Fatalf("%s 应通过白名单: %v", raw, err)
		}
	}
	blocked := []string{
		"https://pixhost.to.evil.com/x.jpg",
		"https://evilpixhost.to/x.jpg",
		"",
	}
	for _, raw := range blocked {
		if err := validateImageProxyURL(raw); err == nil {
			t.Fatalf("%s 应被拒绝", raw)
		}
	}
}
