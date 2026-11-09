package app

import (
	"sync"
	"time"
)

// streamKeepalive 在流式请求等上游时定期往连接上写一点客户端会忽略的东西（#70）。
//
// 为什么要：上游出第一个字前我们一个字节都不写，nginx（proxy_read_timeout 默认 60s）、
// Cloudflare、newapi 这类中间层看连接一直没动静，就自己回 504 把它掐断——我们这边
// 其实还在等、最后也成功了。
//
// 代价：开流要先发 200 响应头，之后再失败就改不了状态码，只能在流里报错。所以默认关，
// 且只在连续空闲满一个间隔才发第一下——秒级就失败的请求（池满、降级、prompt 超长）
// 照样回正确的 HTTP 状态码，被锁成 200 的只有真正慢的那些。
//
// 并发：ping 在后台 goroutine 里写，正文在请求 goroutine 的 onDelta/onReasoning 里写，
// 两边都经 mu 串行。调上游返回后必须先 Stop（会等后台退出），之后的收尾和错误处理
// 回到单 goroutine，不用再加锁。
type streamKeepalive struct {
	mu   sync.Mutex
	last time.Time
	stop chan struct{}
	done chan struct{}
}

// startKeepalive 按 stream_keepalive_sec 起一个保活；设置为 0 时返回 nil（nil 上的方法都是空操作）。
func startKeepalive(ping func()) *streamKeepalive {
	sec := rtCfg().StreamKeepaliveSec
	if sec <= 0 {
		return nil
	}
	return startKeepaliveEvery(time.Duration(sec)*time.Second, ping)
}

func startKeepaliveEvery(interval time.Duration, ping func()) *streamKeepalive {
	k := &streamKeepalive{last: time.Now(), stop: make(chan struct{}), done: make(chan struct{})}
	tick := interval / 4
	if tick > time.Second {
		tick = time.Second
	}
	go func() {
		defer close(k.done)
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-k.stop:
				return
			case now := <-t.C:
				k.mu.Lock()
				if now.Sub(k.last) >= interval {
					ping()
					k.last = now
				}
				k.mu.Unlock()
			}
		}
	}()
	return k
}

// guard 包住往连接写正文的回调：跟 ping 互斥，并把写入时间算作活动（有正文在流就不用 ping）。
func (k *streamKeepalive) guard(f func(string)) func(string) {
	if k == nil || f == nil {
		return f
	}
	return func(s string) {
		k.mu.Lock()
		defer k.mu.Unlock()
		f(s)
		k.last = time.Now()
	}
}

// Stop 停掉后台并等它退出。可重复调用。
func (k *streamKeepalive) Stop() {
	if k == nil {
		return
	}
	k.mu.Lock()
	select {
	case <-k.stop:
	default:
		close(k.stop)
	}
	k.mu.Unlock()
	<-k.done
}
