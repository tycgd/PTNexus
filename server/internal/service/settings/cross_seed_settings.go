package settings

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	processingrepair "github.com/pt-nexus/server/internal/service/processing/repair"
)

// toFloatWithDefault 解析浮点数配置，兼容 int/float/字符串/json.Number，无法解析时回退默认值。
func toFloatWithDefault(value any, fallback float64) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int32:
		return float64(typed)
	case int64:
		return float64(typed)
	case json.Number:
		if parsed, err := typed.Float64(); err == nil {
			return parsed
		}
	case string:
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64); err == nil {
			return parsed
		}
	}
	return fallback
}

func (s *SettingsService) GetCrossSeedSettings() map[string]any {
	defaults := map[string]any{
		"image_hoster":                     "pixhost",
		"screenshot_count":                 3,
		"montage_enabled":                  true,
		"montage_columns":                  3,
		"montage_count":                    9,
		"montage_header":                   true,
		"montage_min_gap":                  8,
		"pixhost_domain":                   "img2.pixhost.cc",
		"agsv_email":                       "",
		"agsv_password":                    "",
		"image_hoster_failure_policy":      "retry",
		"default_downloader":               "",
		"auto_add_existing_to_downloader":  true,
		"auto_update_existing_torrent":     false,
		"publish_batch_concurrency_mode":   "cpu",
		"publish_batch_concurrency_manual": BatchPublishDefaultConcurrency,
		"cspt_ptgen_token":                 "",
		"doubaninfo_appkey":                "",
	}
	cfg := s.cfg.Get()
	merged := mergeWithDefault(defaults, nestedMap(cfg, "cross_seed"))
	// PTGen 节点以配置顺序为准，并补齐新增内置节点，供前端展示名称与说明。
	merged["ptgen_nodes"] = processingrepair.EnrichPTGenNodeSettings(merged["ptgen_nodes"])
	return merged
}

func (s *SettingsService) SaveCrossSeedSettings(newSettings map[string]any) error {
	cfg := s.cfg.Get()
	existing := nestedMap(cfg, "cross_seed")
	merged := mergeWithDefault(existing, newSettings)

	mode := toString(merged["publish_batch_concurrency_mode"], "cpu")
	if mode != "cpu" && mode != "manual" && mode != "all" {
		mode = "cpu"
	}
	merged["publish_batch_concurrency_mode"] = mode
	manual := toIntWithDefault(merged["publish_batch_concurrency_manual"], BatchPublishDefaultConcurrency)
	if manual < 1 {
		manual = 1
	}
	merged["publish_batch_concurrency_manual"] = manual
	merged["auto_add_existing_to_downloader"] = toBool(merged["auto_add_existing_to_downloader"], true)
	merged["auto_update_existing_torrent"] = toBool(merged["auto_update_existing_torrent"], false)
	screenshotCount := toIntWithDefault(merged["screenshot_count"], 3)
	if screenshotCount < 1 {
		screenshotCount = 1
	}
	if screenshotCount > 10 {
		screenshotCount = 10
	}
	merged["screenshot_count"] = screenshotCount
	// 拼图配置归一化：开关/列数(1~4)/帧数(1~20)/顶部 header/最小间隔(≥0)；缺键回退默认，不影响旧字段。
	merged["montage_enabled"] = toBool(merged["montage_enabled"], true)
	montageColumns := toIntWithDefault(merged["montage_columns"], 3)
	if montageColumns < 1 {
		montageColumns = 1
	}
	if montageColumns > 4 {
		montageColumns = 4
	}
	merged["montage_columns"] = montageColumns
	montageCount := toIntWithDefault(merged["montage_count"], 9)
	if montageCount < 1 {
		montageCount = 1
	}
	if montageCount > 20 {
		montageCount = 20
	}
	merged["montage_count"] = montageCount
	merged["montage_header"] = toBool(merged["montage_header"], true)
	montageMinGap := toFloatWithDefault(merged["montage_min_gap"], 8)
	if montageMinGap < 0 {
		montageMinGap = 0
	}
	merged["montage_min_gap"] = montageMinGap
	if toString(merged["image_hoster"], "") == "" {
		merged["image_hoster"] = "pixhost"
	}
	// 图床上传失败策略只允许 retry（默认，失败报错）与 fallback_pixhost（显式回退），非法值回退 retry。
	policy := strings.TrimSpace(toString(merged["image_hoster_failure_policy"], "retry"))
	if policy != "retry" && policy != "fallback_pixhost" {
		policy = "retry"
	}
	merged["image_hoster_failure_policy"] = policy
	merged["pixhost_domain"] = normalizePixhostDomainSetting(merged["pixhost_domain"])
	// PTGen 节点仅持久化 id 与启停状态，数组顺序即优先级。
	merged["ptgen_nodes"] = processingrepair.NormalizePTGenNodeSettingsPayload(merged["ptgen_nodes"])

	cfg["cross_seed"] = merged
	return s.cfg.Save(cfg)
}

func normalizePixhostDomainSetting(value any) string {
	domain := strings.TrimSpace(toString(value, ""))
	if domain == "" {
		return "img2.pixhost.cc"
	}
	domain = strings.TrimPrefix(domain, "http://")
	domain = strings.TrimPrefix(domain, "https://")
	if i := strings.IndexAny(domain, "/?#"); i >= 0 {
		domain = domain[:i]
	}
	domain = strings.ToLower(strings.Trim(domain, ". "))
	if domain == "" {
		return "img2.pixhost.cc"
	}
	if strings.HasPrefix(domain, "api.") {
		domain = "img2." + strings.TrimPrefix(domain, "api.")
	}
	if domain == "pixhost.to" || domain == "pixhost.cc" {
		domain = "img2." + domain
	}
	return domain
}

func (s *SettingsService) PublishConcurrencyInfo() map[string]any {
	cpuThreads := 1
	if detected := runtime.NumCPU(); detected > 0 {
		cpuThreads = detected
	}
	suggested := cpuThreads * 2
	effective := suggested
	if effective > BatchPublishMaxConcurrency {
		effective = BatchPublishMaxConcurrency
	}
	if effective < 1 {
		effective = 1
	}

	return map[string]any{
		"success":                         true,
		"cpu_threads":                     cpuThreads,
		"suggested_concurrency":           suggested,
		"effective_suggested_concurrency": effective,
		"max_concurrency":                 BatchPublishMaxConcurrency,
		"default_concurrency":             BatchPublishDefaultConcurrency,
	}
}

func (s *SettingsService) GetSourcePriority() []any {
	cfg := s.cfg.Get()
	return toSlice(cfg["source_priority"])
}

func (s *SettingsService) SaveSourcePriority(items []any) error {
	for _, item := range items {
		if _, ok := item.(string); !ok {
			return fmt.Errorf("源站点优先级数组中的元素必须是字符串")
		}
	}
	cfg := s.cfg.Get()
	cfg["source_priority"] = items
	return s.cfg.Save(cfg)
}

func (s *SettingsService) GetBatchFetchFilters() map[string]any {
	defaults := map[string]any{"paths": []any{}, "states": []any{}, "downloaderIds": []any{}}
	cfg := s.cfg.Get()
	return mergeWithDefault(defaults, nestedMap(cfg, "batch_fetch_filters"))
}

func (s *SettingsService) SaveBatchFetchFilters(filters map[string]any) error {
	required := []string{"paths", "states", "downloaderIds"}
	for _, key := range required {
		value, exists := filters[key]
		if !exists {
			return fmt.Errorf("缺少必需字段: %s", key)
		}
		if _, ok := value.([]any); !ok {
			return fmt.Errorf("字段 %s 必须是数组", key)
		}
	}
	cfg := s.cfg.Get()
	cfg["batch_fetch_filters"] = filters
	return s.cfg.Save(cfg)
}

func (s *SettingsService) GetCrossSeedReviewFilter() string {
	cfg := s.cfg.Get()
	crossSeed := nestedMap(cfg, "cross_seed")
	return toString(crossSeed["review_filter"], "")
}

func (s *SettingsService) SaveCrossSeedReviewFilter(filter string) error {
	valid := map[string]struct{}{"": {}, "reviewed": {}, "unreviewed": {}, "error": {}}
	if _, ok := valid[filter]; !ok {
		return fmt.Errorf("无效的检查状态筛选值: %s", filter)
	}
	cfg := s.cfg.Get()
	crossSeed := ensureMap(cfg, "cross_seed")
	crossSeed["review_filter"] = filter
	cfg["cross_seed"] = crossSeed
	return s.cfg.Save(cfg)
}

func (s *SettingsService) GetTagsConfig() map[string]any {
	defaults := map[string]any{
		"category": map[string]any{"enabled": true, "category": ""},
		"tags":     map[string]any{"enabled": true, "tags": []any{"PT Nexus", "站点/{站点名称}"}},
	}
	cfg := s.cfg.Get()
	return mergeWithDefault(defaults, nestedMap(cfg, "tags_config"))
}

func (s *SettingsService) SaveTagsConfig(data map[string]any) error {
	category, categoryOK := data["category"].(map[string]any)
	tags, tagsOK := data["tags"].(map[string]any)
	if !categoryOK {
		category = map[string]any{"enabled": true, "category": ""}
	}
	if !tagsOK {
		tags = map[string]any{"enabled": true, "tags": []any{"PT Nexus", "站点/{站点名称}"}}
	}
	cfg := s.cfg.Get()
	cfg["tags_config"] = map[string]any{"category": category, "tags": tags}
	return s.cfg.Save(cfg)
}
