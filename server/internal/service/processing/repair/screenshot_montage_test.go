package repair

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

// 用例：拼图选点数量正确、与正式点及彼此间隔均 ≥ minGap，且为确定性结果。
func TestBuildMontagePointsCountAndSpacing(t *testing.T) {
	duration := 7200.0
	formal := []float64{100, 200, 300}
	got := buildMontagePoints(duration, formal, 9, 8)
	if len(got) != 9 {
		t.Fatalf("拼图点数=%d, 期望 9（points=%v）", len(got), got)
	}
	for _, point := range got {
		if point <= 0 || point >= duration {
			t.Fatalf("拼图点越界: %v", point)
		}
		for _, f := range formal {
			if math.Abs(point-f) < 8-1e-9 {
				t.Fatalf("拼图点 %v 与正式点 %v 距离 < minGap", point, f)
			}
		}
	}
	for i := 0; i < len(got); i++ {
		for j := i + 1; j < len(got); j++ {
			if math.Abs(got[i]-got[j]) < 8-1e-9 {
				t.Fatalf("拼图点 %v 与 %v 距离 < minGap", got[i], got[j])
			}
		}
	}
	again := buildMontagePoints(duration, formal, 9, 8)
	if !reflect.DeepEqual(got, again) {
		t.Fatalf("拼图选点应为确定性结果: %v != %v", got, again)
	}
}

// 用例：minGap 过大致使可用点不足时，依次减半后仍能补足到期望数量。
func TestBuildMontagePointsHalvesGap(t *testing.T) {
	// 桶宽约 4.44s，minGap=8 首轮仅 1 个点，减半到 4 后可取满 9 个。
	got := buildMontagePoints(40, nil, 9, 8)
	if len(got) != 9 {
		t.Fatalf("减半重试后点数=%d, 期望 9（points=%v）", len(got), got)
	}
}

// 用例：正式点过密导致无法满足时，优雅返回较少帧（不足 2 帧，调用方放弃拼图）。
func TestBuildMontagePointsDenseFormalDegrades(t *testing.T) {
	var formal []float64
	for v := 0.0; v <= 100; v += 2 {
		formal = append(formal, v)
	}
	got := buildMontagePoints(100, formal, 9, 8)
	if len(got) >= 2 {
		t.Fatalf("正式点过密时不应凑出 ≥2 帧, 实际 %d（points=%v）", len(got), got)
	}
}

// 用例：非法输入返回 nil。
func TestBuildMontagePointsInvalidInput(t *testing.T) {
	if got := buildMontagePoints(0, nil, 9, 8); got != nil {
		t.Fatalf("duration=0 应返回 nil, 实际 %v", got)
	}
	if got := buildMontagePoints(100, nil, 0, 8); got != nil {
		t.Fatalf("want=0 应返回 nil, 实际 %v", got)
	}
}

// 用例：网格行数为 ceil(count/columns)。
func TestMontageRows(t *testing.T) {
	cases := []struct {
		count, columns, want int
	}{
		{9, 3, 3},
		{8, 3, 3},
		{4, 3, 2},
		{1, 4, 1},
		{0, 3, 0},
		{5, 0, 5},
	}
	for _, tc := range cases {
		if got := montageRows(tc.count, tc.columns); got != tc.want {
			t.Fatalf("montageRows(%d,%d)=%d, 期望 %d", tc.count, tc.columns, got, tc.want)
		}
	}
}

// 用例：拼图配置默认值与越界归一化。
func TestMontageConfigFromConfig(t *testing.T) {
	def := montageConfigFromConfig(nil)
	want := montageConfig{Enabled: true, Columns: 3, Count: 9, Header: true, MinGap: 8}
	if def != want {
		t.Fatalf("默认配置=%+v, 期望 %+v", def, want)
	}

	cfg := montageConfigFromConfig(map[string]any{
		"cross_seed": map[string]any{
			"montage_enabled": false,
			"montage_columns": 10,
			"montage_count":   0,
			"montage_header":  false,
			"montage_min_gap": -3,
		},
	})
	if cfg.Enabled {
		t.Fatal("montage_enabled=false 应生效")
	}
	if cfg.Columns != 4 {
		t.Fatalf("columns 应夹到 4, 实际 %d", cfg.Columns)
	}
	if cfg.Count != 1 {
		t.Fatalf("count 应夹到 1, 实际 %d", cfg.Count)
	}
	if cfg.Header {
		t.Fatal("montage_header=false 应生效")
	}
	if cfg.MinGap != 8 {
		t.Fatalf("负 min_gap 应回退默认 8, 实际 %v", cfg.MinGap)
	}
}

// 用例：parseBoolAny 兼容多种类型。
func TestParseBoolAny(t *testing.T) {
	cases := []struct {
		value any
		want  bool
		ok    bool
	}{
		{true, true, true},
		{false, false, true},
		{"TRUE", true, true},
		{"off", false, true},
		{1, true, true},
		{0, false, true},
		{2.5, true, true},
		{"unknown", false, false},
		{nil, false, false},
	}
	for _, tc := range cases {
		got, ok := parseBoolAny(tc.value)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("parseBoolAny(%v)=(%v,%v), 期望 (%v,%v)", tc.value, got, ok, tc.want, tc.ok)
		}
	}
}

// 用例：drawtext 文本转义剔除换行、转义特殊字符、超长截断。
func TestEscapeDrawText(t *testing.T) {
	got := escapeDrawText("A:B 'C' [x], y;\nZ")
	if strings.ContainsAny(got, "\n\r") {
		t.Fatalf("转义结果不应含换行: %q", got)
	}
	for _, want := range []string{`\:`, `\'`, `\[`, `\,`} {
		if !strings.Contains(got, want) {
			t.Fatalf("转义结果缺少 %q: %q", want, got)
		}
	}
	long := escapeDrawText(strings.Repeat("a", 300))
	if len([]rune(long)) != montageHeaderMaxRunes {
		t.Fatalf("超长文本应截断到 %d runes, 实际 %d", montageHeaderMaxRunes, len([]rune(long)))
	}
	if escapeDrawText("   ") != "" {
		t.Fatal("空白文本应返回空串")
	}
}

// 用例：mediainfo 压缩成单行关键信息与空/退化输入。
func TestBuildMediaInfoHeaderLine(t *testing.T) {
	if got := buildMediaInfoHeaderLine("   "); got != "" {
		t.Fatalf("空输入应返回 \"\", 实际 %q", got)
	}

	sample := strings.Join([]string{
		"General",
		"Complete name : /movies/Test.Movie.2024.1080p.BluRay.mkv",
		"Format : Matroska",
		"Duration : 1 h 58 min",
		"",
		"Video",
		"Format : AVC",
		"Width : 1 920 pixels",
		"Height : 1 080 pixels",
		"",
		"Audio",
		"Format : DTS",
		"Channel(s) : 6 channels",
	}, "\n")
	got := buildMediaInfoHeaderLine(sample)
	if strings.ContainsAny(got, "\n\r") {
		t.Fatalf("header 应为单行: %q", got)
	}
	for _, want := range []string{"Test.Movie.2024.1080p.BluRay.mkv", "1 h 58 min", "1920x1080", "AVC", "DTS"} {
		if !strings.Contains(got, want) {
			t.Fatalf("header 缺少 %q: %q", want, got)
		}
	}

	fallback := buildMediaInfoHeaderLine("Some plain first line\nsecond line")
	if strings.TrimSpace(fallback) == "" || strings.ContainsAny(fallback, "\n\r") {
		t.Fatalf("退化输入应返回非空单行, 实际 %q", fallback)
	}
}

// 用例：concat+tile 滤镜前段拼接正确。
func TestBuildMontageConcatTileFilter(t *testing.T) {
	got := buildMontageConcatTileFilter(9, 3, 3)
	want := "[0:v][1:v][2:v][3:v][4:v][5:v][6:v][7:v][8:v]concat=n=9:v=1:a=0,tile=3x3[grid]"
	if got != want {
		t.Fatalf("滤镜=%q, 期望 %q", got, want)
	}
}
