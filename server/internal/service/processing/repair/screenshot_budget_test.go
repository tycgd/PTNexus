package repair

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeFakePNGHeader 写入一个仅含 24 字节 PNG 文件头（含 IHDR 宽度）的临时文件。
func writeFakePNGHeader(t *testing.T, width uint32) string {
	t.Helper()
	header := make([]byte, 24)
	copy(header, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})
	binary.BigEndian.PutUint32(header[16:20], width)
	path := filepath.Join(t.TempDir(), "fake.png")
	if err := os.WriteFile(path, header, 0o644); err != nil {
		t.Fatalf("写入假 PNG 头失败: %v", err)
	}
	return path
}

// 用例：宽度阶梯选择。仅当超过硬上限时才下采样；跳过不小于当前宽度的档位。
func TestPickDownscalePlan(t *testing.T) {
	cases := []struct {
		name  string
		width int
		size  int64
		want  []int
	}{
		{"1080p_4MB", 1920, 4 * 1024 * 1024, []int{1280, 1024, 800, 640}},
		{"4K_12MB", 3840, 12 * 1024 * 1024, []int{1920, 1280, 1024, 800, 640}},
		{"720p_800KB_未超硬上限", 1280, 800 * 1024, nil},
		{"640宽_2MB_无更小档", 640, 2 * 1024 * 1024, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pickDownscalePlan(tc.width, tc.size)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("pickDownscalePlan(%d,%d) = %v, 期望 %v", tc.width, tc.size, got, tc.want)
			}
		})
	}
}

// 用例：PNG IHDR 宽度解析与非法输入报错。
func TestPNGWidth(t *testing.T) {
	path := writeFakePNGHeader(t, 1920)
	width, err := pngWidth(path)
	if err != nil {
		t.Fatalf("解析合法 PNG 头失败: %v", err)
	}
	if width != 1920 {
		t.Fatalf("pngWidth=%d, 期望 1920", width)
	}

	badPath := filepath.Join(t.TempDir(), "bad.png")
	if err := os.WriteFile(badPath, []byte("this is not a png file at all"), 0o644); err != nil {
		t.Fatalf("写入非法文件失败: %v", err)
	}
	if _, err := pngWidth(badPath); err == nil {
		t.Fatal("非 PNG 文件应返回错误")
	}

	shortPath := filepath.Join(t.TempDir(), "short.png")
	if err := os.WriteFile(shortPath, []byte{0x89, 'P', 'N', 'G'}, 0o644); err != nil {
		t.Fatalf("写入截断文件失败: %v", err)
	}
	if _, err := pngWidth(shortPath); err == nil {
		t.Fatal("头部不完整的文件应返回错误")
	}
}

// 用例：文件未超硬上限时预算函数直接返回，不依赖 ffmpeg。
func TestEnsureScreenshotWithinBudgetSmallFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "small.png")
	if err := os.WriteFile(path, []byte("small"), 0o644); err != nil {
		t.Fatalf("写入小文件失败: %v", err)
	}
	size, err := ensureScreenshotWithinBudget("ffmpeg-not-installed", path)
	if err != nil {
		t.Fatalf("小文件应直接返回，不应报错: %v", err)
	}
	if size != int64(len("small")) {
		t.Fatalf("返回字节=%d, 期望 %d", size, len("small"))
	}
}

// 用例：真实 ffmpeg 重编码集成（无 ffmpeg 时跳过）。
func TestDownscalePNGToWidthIntegration(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		if env := strings.TrimSpace(os.Getenv("PTNEXUS_FFMPEG_PATH")); env != "" {
			ffmpegPath = env
		} else {
			t.Skip("环境无 ffmpeg，跳过重编码集成用例")
		}
	}

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.png")
	genArgs := []string{
		"-y", "-v", "error",
		"-f", "lavfi", "-i", "color=c=red:s=2000x1000:d=1",
		"-frames:v", "1",
		srcPath,
	}
	if out, genErr := exec.Command(ffmpegPath, genArgs...).CombinedOutput(); genErr != nil {
		t.Skipf("ffmpeg 无法生成测试图，跳过集成用例: %v output=%s", genErr, strings.TrimSpace(string(out)))
	}

	srcWidth, err := pngWidth(srcPath)
	if err != nil {
		t.Fatalf("读取源图宽度失败: %v", err)
	}
	if srcWidth != 2000 {
		t.Fatalf("源图宽度=%d, 期望 2000", srcWidth)
	}

	dstPath := filepath.Join(dir, "dst.png")
	if err := downscalePNGToWidth(ffmpegPath, srcPath, dstPath, 1024); err != nil {
		t.Fatalf("downscalePNGToWidth 失败: %v", err)
	}
	dstWidth, err := pngWidth(dstPath)
	if err != nil {
		t.Fatalf("读取目标图宽度失败: %v", err)
	}
	if dstWidth != 1024 {
		t.Fatalf("目标图宽度=%d, 期望 1024", dstWidth)
	}
	if fileSizeBytes(dstPath) <= 0 {
		t.Fatal("目标图不应为空文件")
	}
}
