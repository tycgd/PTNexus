package repair

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pt-nexus/server/internal/platform/logx"
)

// 拼图（contact sheet）相关常量：3×3 网格 + 顶部 mediainfo 一行。
const (
	defaultMontageColumns = 3
	defaultMontageCount   = 9
	defaultMontageMinGap  = 8.0
	maxMontageColumns     = 4
	maxMontageCount       = 20

	// 单个拼图格子归一化后的目标尺寸（16:9，强制 increase+crop）。
	montageTileWidth  = 640
	montageTileHeight = 360
	// 顶部 mediainfo 文字条高度与字号。
	montageHeaderHeight = 56
	montageFontSize     = 26
	// 拼图 header 文字最大展示长度（超出截断）。
	// header 宽 1920px、字号 26px：ASCII 约 26px/字，70 字≈1820px<1920；中英混排按 ~70 保守截断，
	// 避免 drawtext 右侧溢出被裁（原 120 对中文过宽）。
	montageHeaderMaxRunes = 70
)

// montageFontCandidates 为顶部 header 可选的中文字体（镜像已装 fonts-noto-cjk），缺失则跳过 header。
var montageFontCandidates = []string{
	"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
	"/usr/share/fonts/truetype/noto/NotoSansCJK-Regular.ttc",
	"/usr/share/fonts/opentype/noto/NotoSansCJK-Bold.ttc",
}

// montageConfig 描述拼图功能的运行时配置。
type montageConfig struct {
	Enabled bool
	Columns int
	Count   int
	Header  bool
	MinGap  float64
}

// montageConfigFromConfig 从根配置读取拼图配置，缺失键回退默认值（不影响旧字段行为）。
// 参数/返回：rootConfig 为应用根配置；返回归一化后的 montageConfig（columns 1~4、count 1~20、min_gap≥0）。
// 失败场景：配置结构或数值无法解析时使用默认值。
// 副作用：仅读取内存中的配置。
func montageConfigFromConfig(rootConfig map[string]any) montageConfig {
	cfg := montageConfig{
		Enabled: true,
		Columns: defaultMontageColumns,
		Count:   defaultMontageCount,
		Header:  true,
		MinGap:  defaultMontageMinGap,
	}
	if rootConfig == nil {
		return cfg
	}
	crossSeed, ok := rootConfig["cross_seed"].(map[string]any)
	if !ok {
		return cfg
	}
	if value, ok := parseBoolAny(crossSeed["montage_enabled"]); ok {
		cfg.Enabled = value
	}
	if value, ok := parseIntAny(crossSeed["montage_columns"]); ok {
		cfg.Columns = clampIntRange(value, 1, maxMontageColumns)
	}
	if value, ok := parseIntAny(crossSeed["montage_count"]); ok {
		cfg.Count = clampIntRange(value, 1, maxMontageCount)
	}
	if value, ok := parseBoolAny(crossSeed["montage_header"]); ok {
		cfg.Header = value
	}
	if value, ok := parseFloatAny(crossSeed["montage_min_gap"]); ok && value >= 0 {
		cfg.MinGap = value
	}
	return cfg
}

// parseBoolAny 解析任意类型的布尔值（兼容 bool/int/float/字符串/json.Number）。
// 参数/返回：value 为待解析值；返回值与是否解析成功。
// 失败场景：无法识别的类型或字符串返回 (false, false)。
// 副作用：无。
func parseBoolAny(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case int:
		return typed != 0, true
	case int32:
		return typed != 0, true
	case int64:
		return typed != 0, true
	case float32:
		return typed != 0, true
	case float64:
		return typed != 0, true
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return parsed != 0, true
		}
		if parsed, err := typed.Float64(); err == nil {
			return parsed != 0, true
		}
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "1", "yes", "on":
			return true, true
		case "false", "0", "no", "off":
			return false, true
		}
	}
	return false, false
}

func clampIntRange(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

// montageRows 计算网格行数：rows = ceil(count/columns)。
func montageRows(count, columns int) int {
	if count <= 0 {
		return 0
	}
	if columns < 1 {
		columns = 1
	}
	return (count + columns - 1) / columns
}

// buildMontagePoints 按时间轴均匀分桶取桶中点，选出与正式点及彼此间隔 ≥ minGap 的拼图时间点。
// 功能：确定性、无随机；不足 want 时按 minGap 依次减半（8→4→2→1）重试，仍不足返回可得较少帧。
// 参数/返回：duration 视频时长（秒）；formalPoints 正式截图时间点；want 期望拼图帧数；minGap 最小间隔（秒）。
// 失败场景：duration/want 非正时返回 nil。
// 副作用：无（纯函数）。
func buildMontagePoints(duration float64, formalPoints []float64, want int, minGap float64) []float64 {
	if duration <= 0 || want <= 0 {
		return nil
	}
	gap := minGap
	if gap < 0 {
		gap = 0
	}

	bucket := duration / float64(want)
	candidates := make([]float64, 0, want)
	for i := 0; i < want; i++ {
		point := bucket * (float64(i) + 0.5)
		if duration > 2 {
			if point < 1 {
				point = 1
			}
			if point > duration-1 {
				point = duration - 1
			}
		}
		if point <= 0 || point >= duration {
			continue
		}
		candidates = append(candidates, point)
	}
	if len(candidates) == 0 {
		return nil
	}

	selectWithGap := func(g float64) []float64 {
		selected := make([]float64, 0, len(candidates))
		for _, candidate := range candidates {
			if hasPointWithinGap(candidate, formalPoints, g) {
				continue
			}
			if hasPointWithinGap(candidate, selected, g) {
				continue
			}
			selected = append(selected, candidate)
		}
		return selected
	}

	attemptGaps := make([]float64, 0, 5)
	for g := gap; g > 1; g /= 2 {
		attemptGaps = append(attemptGaps, g)
	}
	attemptGaps = append(attemptGaps, 1)
	if gap == 0 {
		attemptGaps = []float64{0}
	}

	var best []float64
	for _, g := range attemptGaps {
		result := selectWithGap(g)
		if len(result) >= want {
			sort.Float64s(result)
			return result[:want]
		}
		if len(result) > len(best) {
			best = result
		}
	}
	sort.Float64s(best)
	return best
}

// hasPointWithinGap 判断 point 与列表任意一点的距离是否小于 gap。
func hasPointWithinGap(point float64, points []float64, gap float64) bool {
	if gap <= 0 {
		return false
	}
	for _, existing := range points {
		if math.Abs(point-existing) < gap {
			return true
		}
	}
	return false
}

// montageRequest 汇总生成一次拼图所需的上下文。
type montageRequest struct {
	ffmpegPath   string
	mpvPath      string
	videoPath    string
	formalPoints []float64
	subtitleSID  int
	isHDR        bool
	config       montageConfig
	uploadCtx    ScreenshotUploadContext
	mediaText    string
	tmpDir       string
}

// tryGenerateAndUploadMontage 生成拼图并上传，任何一步失败都只告警并返回 ok=false（绝不让整单失败）。
// 参数/返回：req 为拼图上下文；返回拼图 URL 与是否成功。
// 失败场景：选点不足（<2 帧）、抓帧/拼接/字体/上传失败均返回 ("", false)。
// 副作用：执行 mpv/ffmpeg 抓帧与拼接、读取视频、向图床发起上传。
func tryGenerateAndUploadMontage(req montageRequest) (string, bool) {
	if req.config.Count < 2 {
		logx.Warnf(screenshotPreviewLogModule, "跳过截图拼图：montage_count=%d 不足 2", req.config.Count)
		return "", false
	}
	duration, err := probeDurationSeconds(req.videoPath)
	if err != nil || duration <= 0 {
		logx.Warnf(screenshotPreviewLogModule, "跳过截图拼图：读取视频时长失败 err=%v", err)
		return "", false
	}

	points := buildMontagePoints(duration, req.formalPoints, req.config.Count, req.config.MinGap)
	if len(points) < 2 {
		logx.Warnf(screenshotPreviewLogModule, "跳过截图拼图：可用拼图时间点不足（%d，要求≥2）", len(points))
		return "", false
	}

	montageDir := filepath.Join(req.tmpDir, "montage")
	if mkErr := os.MkdirAll(montageDir, 0o755); mkErr != nil {
		logx.Warnf(screenshotPreviewLogModule, "跳过截图拼图：创建临时目录失败 err=%v", mkErr)
		return "", false
	}

	frames := make([]string, 0, len(points))
	for idx, point := range points {
		dst := filepath.Join(montageDir, fmt.Sprintf("frame_%02d.png", idx+1))
		if capErr := captureFormalFramePNG(req.mpvPath, req.ffmpegPath, req.videoPath, point, req.subtitleSID, req.isHDR, dst); capErr != nil {
			logx.Warnf(screenshotPreviewLogModule, "拼图抓帧失败 index=%d point=%.2f err=%v", idx, point, capErr)
			continue
		}
		frames = append(frames, dst)
	}
	if len(frames) < 2 {
		logx.Warnf(screenshotPreviewLogModule, "跳过截图拼图：成功抓帧不足（%d，要求≥2）", len(frames))
		return "", false
	}

	headerText := ""
	if req.config.Header {
		headerText = buildMediaInfoHeaderLine(req.mediaText)
	}
	fontFile := montageFontFile()

	// 最终拼图文件名必须唯一：不同视频的拼图若同名会在图床互相覆盖并串缓存。
	montagePNG := filepath.Join(montageDir, fmt.Sprintf("montage_%d.png", time.Now().UnixNano()))
	if asmErr := assembleMontagePNG(req.ffmpegPath, frames, req.config.Columns, headerText, fontFile, montagePNG); asmErr != nil {
		logx.Warnf(screenshotPreviewLogModule, "截图拼图拼接失败，降级只出单图 err=%v", asmErr)
		return "", false
	}
	if _, budgetErr := ensureScreenshotWithinBudget(req.ffmpegPath, montagePNG); budgetErr != nil {
		logx.Warnf(screenshotPreviewLogModule, "截图拼图尺寸预算处理失败，降级只出单图 err=%v", budgetErr)
		return "", false
	}

	showURL, upErr := req.uploadCtx.UploadScreenshot(montagePNG, func(format string, args ...any) {
		logx.PlainInfof(format, args...)
	})
	if upErr != nil || strings.TrimSpace(showURL) == "" {
		logx.Warnf(screenshotPreviewLogModule, "截图拼图上传失败，降级只出单图 err=%v", upErr)
		return "", false
	}
	finalURL := strings.TrimSpace(req.uploadCtx.NormalizeScreenshotURL(showURL))
	if finalURL == "" {
		logx.Warnf(screenshotPreviewLogModule, "截图拼图上传返回空链接，降级只出单图")
		return "", false
	}
	logx.Infof(screenshotPreviewLogModule, "截图拼图生成成功 frames=%d columns=%d header=%t url=%s", len(frames), req.config.Columns, headerText != "", finalURL)
	return finalURL, true
}

// captureFormalFramePNG 抓取一帧正式画质 PNG（统一 HDR 直截 与 mpv+compress 两条管线）。
// 参数/返回：isHDR=true 走 ffmpeg libplacebo 直截；否则走 mpv 抓 raw PNG 再压缩为 SDR PNG。
// 失败场景：mpv/ffmpeg 执行失败或未产出文件时返回错误。
// 副作用：执行外部命令并写 dstPNG（SDR 路径会额外产生同目录 .raw.png）。
func captureFormalFramePNG(mpvPath, ffmpegPath, videoPath string, point float64, subtitleSID int, isHDR bool, dstPNG string) error {
	if isHDR {
		return captureDVPNGWithFFmpeg(ffmpegPath, videoPath, point, dstPNG)
	}
	rawPNG := strings.TrimSuffix(dstPNG, filepath.Ext(dstPNG)) + ".raw.png"
	if err := captureRawPNGWithMPV(mpvPath, videoPath, point, rawPNG, subtitleSID); err != nil {
		return err
	}
	if stat, statErr := os.Stat(rawPNG); statErr != nil || stat.Size() == 0 {
		return fmt.Errorf("mpv 未生成文件: %s", rawPNG)
	}
	// mpv 已将 HDR（含 HDR10/HLG）色调映射为 SDR 像素，此处仅做 PNG 无损压缩，不再二次映射。
	return compressPNGWithFFmpeg(ffmpegPath, rawPNG, dstPNG, false)
}

// assembleMontagePNG 把多帧拼接为 columns×rows 网格 PNG，可选在顶部叠加 mediainfo 文字条。
// 步骤：① 每帧 scale/crop 归一为 montageTileWidth×montageTileHeight；
//
//	② N 个输入 concat → tile=cols x rows 输出网格；headerText 非空且字体存在时用黑底 drawtext 条 vstack 到顶部。
//
// 参数/返回：framePaths 为已抓取的帧；columns 列数；headerText 顶部文字（可空）；fontFile 字体路径（空则跳过 header）；outPNG 输出。
// 失败场景：帧为空、归一化/拼接命令失败或未产出文件时返回错误。
// 副作用：执行 ffmpeg 子进程并在 outPNG 同目录写临时归一化帧。
func assembleMontagePNG(ffmpegPath string, framePaths []string, columns int, headerText, fontFile, outPNG string) error {
	if len(framePaths) == 0 {
		return fmt.Errorf("拼图帧为空")
	}
	if columns < 1 {
		columns = 1
	}
	if columns > maxMontageColumns {
		columns = maxMontageColumns
	}
	rows := montageRows(len(framePaths), columns)
	total := columns * rows
	if total <= 0 {
		return fmt.Errorf("拼图布局非法 columns=%d rows=%d", columns, rows)
	}

	workDir := filepath.Dir(outPNG)
	normalized := make([]string, 0, total)
	for idx, frame := range framePaths {
		if idx >= total {
			break
		}
		dst := filepath.Join(workDir, fmt.Sprintf("norm_%02d.png", idx+1))
		if err := normalizeMontageFrame(ffmpegPath, frame, dst); err != nil {
			return err
		}
		normalized = append(normalized, dst)
	}
	// 补齐不足的格子（黑底），避免 tile 输出残格。
	fillerPath := filepath.Join(workDir, "montage_filler.png")
	for len(normalized) < total {
		if _, statErr := os.Stat(fillerPath); statErr != nil {
			if err := writeMontageFiller(ffmpegPath, fillerPath); err != nil {
				return err
			}
		}
		normalized = append(normalized, fillerPath)
	}

	args := []string{"-y", "-v", "error"}
	for _, frame := range normalized {
		args = append(args, "-i", frame)
	}

	filter := buildMontageConcatTileFilter(total, columns, rows)
	escapedText := escapeDrawText(headerText)
	escapedFont := escapeFilterPath(fontFile)
	if headerText != "" && escapedFont != "" && escapedText != "" {
		headerIdx := len(normalized)
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("color=c=black:s=%dx%d:d=1,format=rgb24", montageTileWidth*columns, montageHeaderHeight))
		filter += fmt.Sprintf(";[%d:v]drawtext=fontfile=%s:text=%s:fontcolor=white:fontsize=%d:x=12:y=(h-%d)/2:expansion=none[hdr];[hdr][grid]vstack=inputs=2[out]",
			headerIdx, escapedFont, escapedText, montageFontSize, montageFontSize)
	} else {
		filter += ";[grid]null[out]"
	}

	args = append(args, "-filter_complex", filter, "-map", "[out]", "-frames:v", "1", outPNG)
	out, err := exec.Command(ffmpegPath, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg 拼图失败: %s", formatFFmpegFailureOutput(string(out), err))
	}
	if stat, statErr := os.Stat(outPNG); statErr != nil || stat.Size() == 0 {
		return fmt.Errorf("ffmpeg 拼图未生成有效文件: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// buildMontageConcatTileFilter 构造 concat + tile 的滤镜前段，输出标签为 [grid]。
func buildMontageConcatTileFilter(total, columns, rows int) string {
	var b strings.Builder
	for i := 0; i < total; i++ {
		fmt.Fprintf(&b, "[%d:v]", i)
	}
	fmt.Fprintf(&b, "concat=n=%d:v=1:a=0,tile=%dx%d[grid]", total, columns, rows)
	return b.String()
}

// normalizeMontageFrame 把单帧 scale/crop 归一为统一格子尺寸。
func normalizeMontageFrame(ffmpegPath, src, dst string) error {
	vf := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,setsar=1,format=rgb24",
		montageTileWidth, montageTileHeight, montageTileWidth, montageTileHeight)
	args := []string{
		"-y", "-v", "error",
		"-i", src,
		"-frames:v", "1",
		"-vf", vf,
		"-compression_level", "3",
		dst,
	}
	out, err := exec.Command(ffmpegPath, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg 归一化拼图帧失败: %s", formatFFmpegFailureOutput(string(out), err))
	}
	if stat, statErr := os.Stat(dst); statErr != nil || stat.Size() == 0 {
		return fmt.Errorf("拼图帧归一化未生成有效文件")
	}
	return nil
}

// writeMontageFiller 生成一张黑底格子占位图。
func writeMontageFiller(ffmpegPath, dst string) error {
	args := []string{
		"-y", "-v", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("color=c=black:s=%dx%d:d=1,format=rgb24", montageTileWidth, montageTileHeight),
		"-frames:v", "1",
		dst,
	}
	out, err := exec.Command(ffmpegPath, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg 生成拼图占位帧失败: %s", formatFFmpegFailureOutput(string(out), err))
	}
	return nil
}

// montageFontFile 返回首个存在的中文 header 字体路径，均不存在时返回空串（跳过 header）。
func montageFontFile() string {
	for _, candidate := range montageFontCandidates {
		if stat, err := os.Stat(candidate); err == nil && !stat.IsDir() {
			return candidate
		}
	}
	return ""
}

// escapeFilterPath 转义 ffmpeg 滤镜选项中的路径特殊字符。
func escapeFilterPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		":", "\\:",
		",", "\\,",
		"'", "\\'",
		"[", "\\[",
		"]", "\\]",
	)
	return replacer.Replace(path)
}

// escapeDrawText 处理 drawtext 文本转义：\ : ' , [ ] ; 以及剔除换行、超长截断。
func escapeDrawText(text string) string {
	normalized := strings.ReplaceAll(text, "\r", " ")
	normalized = strings.ReplaceAll(normalized, "\n", " ")
	normalized = strings.Join(strings.Fields(normalized), " ")
	if normalized == "" {
		return ""
	}
	runes := []rune(normalized)
	if len(runes) > montageHeaderMaxRunes {
		normalized = string(runes[:montageHeaderMaxRunes])
	}
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		":", "\\:",
		"'", "\\'",
		",", "\\,",
		"[", "\\[",
		"]", "\\]",
		";", "\\;",
	)
	return replacer.Replace(normalized)
}

// buildMediaInfoHeaderLine 把 MediaInfo/BDInfo 文本压缩成一行关键信息，供拼图 header 使用。
// 优先 File/Duration/Video(分辨率+编码)/Audio(编码+声道)；取不到退回截断的首行；空输入返回 ""。
func buildMediaInfoHeaderLine(mediainfoText string) string {
	text := strings.TrimSpace(mediainfoText)
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")

	getValue := func(keys ...string) string {
		for _, line := range lines {
			key, value, ok := splitMediaInfoKeyValue(line)
			if !ok {
				continue
			}
			for _, wanted := range keys {
				if strings.EqualFold(key, wanted) {
					if strings.TrimSpace(value) != "" {
						return strings.TrimSpace(value)
					}
				}
			}
		}
		return ""
	}

	parts := make([]string, 0, 4)

	if fileName := getValue("Complete name", "File name", "FileName", "Disc Title", "Disc Label", "Label"); fileName != "" {
		parts = append(parts, compactMediaFileName(fileName))
	}

	if duration := getValue("Duration"); duration != "" {
		parts = append(parts, duration)
	}

	resolution := ""
	if width := leadingDigits(getValue("Width")); width != "" {
		if height := leadingDigits(getValue("Height")); height != "" {
			resolution = width + "x" + height
		}
	}
	if resolution == "" {
		lower := strings.ToLower(text)
		for _, token := range []string{"2160p", "1080p", "1080i", "720p", "576p", "480p"} {
			if strings.Contains(lower, token) {
				resolution = token
				break
			}
		}
	}
	videoCodec := firstTokenMatch(text, []string{"HEVC", "AVC", "VC-1", "MPEG-2", "AV1", "VP9", "H.265", "H.264"})
	if resolution != "" || videoCodec != "" {
		parts = append(parts, strings.TrimSpace(resolution+" "+videoCodec))
	}

	audioCodec := firstTokenMatch(text, []string{"DTS-HD MA", "DTS-HD", "TrueHD", "Atmos", "DTS", "E-AC-3", "AC-3", "Dolby Digital Plus", "Dolby Digital", "FLAC", "AAC", "LPCM", "PCM", "Opus"})
	channels := firstTokenMatch(text, []string{"7.1", "5.1", "2.0", "1.0"})
	if audioCodec != "" || channels != "" {
		parts = append(parts, strings.TrimSpace(audioCodec+" "+channels))
	}

	if len(parts) > 0 {
		return strings.Join(parts, " | ")
	}

	// 退化：压缩整段文本的首个非空行并截断。
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return CompactLogText(trimmed, montageHeaderMaxRunes)
		}
	}
	return ""
}

// splitMediaInfoKeyValue 按首个 ':' 或 '：' 拆分 MediaInfo/BDInfo 的 key/value 行。
func splitMediaInfoKeyValue(line string) (string, string, bool) {
	idx := strings.IndexAny(line, ":：")
	if idx < 0 {
		return "", "", false
	}
	key := strings.TrimSpace(line[:idx])
	if key == "" {
		return "", "", false
	}
	value := strings.TrimLeft(line[idx:], ":：")
	return key, strings.TrimSpace(value), true
}

// leadingDigits 提取字符串中首段连续数字（忽略空格，兼容 "1 920 pixels"）。
func leadingDigits(value string) string {
	var b strings.Builder
	started := false
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			started = true
		case r == ' ' || r == '\t':
			continue
		default:
			if started {
				return b.String()
			}
			// 首个非数字且非空格：放弃（如 "Unknown"）。
			return ""
		}
	}
	return b.String()
}

// compactMediaFileName 去掉路径前缀，仅保留文件名。
func compactMediaFileName(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	trimmed = strings.ReplaceAll(trimmed, "\\", "/")
	if idx := strings.LastIndex(trimmed, "/"); idx >= 0 && idx < len(trimmed)-1 {
		trimmed = trimmed[idx+1:]
	}
	return trimmed
}

// firstTokenMatch 返回 tokens 中首个在 text 内命中（不区分大小写）的原始大小写 token。
func firstTokenMatch(text string, tokens []string) string {
	lower := strings.ToLower(text)
	for _, token := range tokens {
		if strings.Contains(lower, strings.ToLower(token)) {
			return token
		}
	}
	return ""
}
