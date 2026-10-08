package app

import (
	"regexp"
	"sync"
	"time"
)

// bl 是上游前端的构建版本号，形如 boq_gemini-web-uiserver_20261007.01_p0，
// 每个 StreamGenerate 请求都要带。它埋在 /app 页面 HTML 的 "cfb2h" 字段里。
// （2026-10 上游把服务名从 assistant-bard-web-server 改成了 gemini-web-uiserver，
// 新旧两个值实测都还能用；校验正则已放宽不钉服务名，见 blValueRe。）
//
// 钉死一个值是能用的 —— 实测落后好几个月的旧值仍然正常。但"仍然可用"不等于"永远可用"，
// 而这个值一旦被上游弃用，表现是所有请求大面积失败，排查时很难第一时间想到
// 是个版本号过期了。既然 xsrf.go 本来就在抓 /app 页面，顺手取一下成本几乎为零。
var (
	blMu       sync.RWMutex
	blFetched  string    // 抓到的最新值，空表示还没抓到
	blFetchAt  time.Time // 上次抓取尝试的时间（成功失败都记，避免失败时疯狂重试）
	blFetching bool      // 有一次抓取正在飞
)

// bl 变化的粒度是天，隔几小时看一眼足够。
const blTTL = 6 * time.Hour

var cfb2hRe = regexp.MustCompile(`"cfb2h":"([^"]{10,120})"`)

// blValueRe 卡住形状：boq_<服务名>_<8位日期>.<2位>_p<数字>。
// 页面上的 cfb2h 理论上就是这个格式，但它是外部输入，直接拿去拼 URL 等于让
// 上游页面决定我们发什么请求。形状对不上就当没抓到，继续用配置里钉的值 ——
// 宁可落后，不可乱发。
//
// 服务名那段不钉死（原来钉的是 assistant-bard-web-server）：2026-10 上游把服务
// 改名成 gemini-web-uiserver，钉死服务名会让自动跟随把新值当"形状不对"拒掉，于是
// 永远回退到配置里那个旧值。只校验 boq_ 前缀 + 日期.版本_p 后缀这个总体形状，
// 服务名随它改——够挡住抓错的垃圾，又扛得住改名。
var blValueRe = regexp.MustCompile(`^boq_[a-z0-9-]+_\d{8}\.\d{2}_p\d+$`)

// currentBL 返回这次请求该用的 bl。
//
// 不阻塞：过期时只是踢一次后台抓取，本次仍返回当前值。bl 是缓慢变化的版本号，
// 用旧值最多是落后几个版本（实测落后两个多月仍可用），不值得为它给每个请求
// 加一次页面抓取的延迟。auto 关掉时永远用配置里的值。
func currentBL(proxyURL string) string {
	pinned := rtCfg().GeminiBL
	if !rtCfg().GeminiBLAuto {
		return pinned
	}

	blMu.RLock()
	val, fetchedAt, inflight := blFetched, blFetchAt, blFetching
	blMu.RUnlock()

	if !inflight && time.Since(fetchedAt) > blTTL {
		blMu.Lock()
		// 双检：并发请求下只放一个 goroutine 去抓。
		if !blFetching && time.Since(blFetchAt) > blTTL {
			blFetching = true
			go refreshBL(proxyURL)
		}
		blMu.Unlock()
	}

	if val != "" {
		return val
	}
	return pinned
}

// refreshBL 抓一次 /app 把 cfb2h 取出来。匿名抓即可，这个值跟登录态无关。
func refreshBL(proxyURL string) {
	defer func() {
		blMu.Lock()
		blFetching = false
		blFetchAt = time.Now() // 成功失败都记，失败时也要等满 TTL 再试
		blMu.Unlock()
	}()

	body, err := fetchAppPage("", proxyURL)
	if err != nil {
		logf("[bl] 抓 /app 失败，继续用当前值: %v", err)
		return
	}
	m := cfb2hRe.FindSubmatch(body)
	if m == nil {
		logf("[bl] 页面里没有 cfb2h，继续用当前值")
		return
	}
	got := string(m[1])
	if !blValueRe.MatchString(got) {
		logf("[bl] 抓到的值形状不对，忽略: %q", truncate(got, 60))
		return
	}

	blMu.Lock()
	changed := blFetched != got
	blFetched = got
	blMu.Unlock()
	if changed {
		logf("[bl] 更新为 %s（配置里钉的是 %s）", got, rtCfg().GeminiBL)
	}
}
