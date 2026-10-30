package app

import (
	"os"
	"strings"
	"testing"
)

// 打真实上游的连通性探针，只在设了 GW2A_UPLOAD_PROBE=<代理URL> 时跑。
// 常规 go test 会跳过 —— 上传要页面 token 和真实出口，进不了单测的封闭环境。
func TestUploadProbe(t *testing.T) {
	proxy := os.Getenv("GW2A_UPLOAD_PROBE")
	if proxy == "" {
		t.Skip("未设 GW2A_UPLOAD_PROBE")
	}
	text := "PANTE-UPLOAD-PROBE\n" + strings.Repeat("这是一段用于验证上传链路的文本。\n", 50)
	ref, err := uploadBytes("", 0, proxy, []byte(text), "context.txt")
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	t.Logf("上传成功，引用路径 = %s", ref)
	if !strings.HasPrefix(ref, "/") {
		t.Errorf("引用路径形状不对: %q", ref)
	}
}
