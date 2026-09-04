package app

import (
	"sort"
	"time"
)

// startScheduler kicks off background loops:
//  1. hourly aggregation: rolls up requests in the previous closed hour bucket
//  2. daily aggregation: rolls up the previous closed day bucket
//  3. retention: deletes raw requests older than rtCfg().RetentionDays
//  4. proxy cache reload: every 60s pull proxies from DB
func startScheduler() {
	go func() {
		// On boot: catch up any aggregations missed while service was down.
		aggregateHourlyCatchup()
		aggregateDailyCatchup()
		retentionSweep()

		hourly := time.NewTicker(5 * time.Minute)
		daily := time.NewTicker(1 * time.Hour)
		retention := time.NewTicker(6 * time.Hour)
		proxyReload := time.NewTicker(60 * time.Second)
		// 会话保活：启动后先尽快刷一次 1PSIDTS（导入的票可能已经快到期），
		// 之后按服务端在轮转页里指定的间隔走（实测 600 秒）。
		rotate := time.NewTimer(firstRotateDelay)
		defer hourly.Stop()
		defer daily.Stop()
		defer retention.Stop()
		defer proxyReload.Stop()
		defer rotate.Stop()

		for {
			select {
			case <-hourly.C:
				aggregateHourlyCatchup()
			case <-daily.C:
				aggregateDailyCatchup()
			case <-retention.C:
				retentionSweep()
			case <-proxyReload.C:
				loadProxies()
			case <-rotate.C:
				rotate.Reset(rotateAllAccounts())
			}
		}
	}()
}

// aggregateHourlyCatchup walks every hour bucket from the latest aggregated
// hour up to (now - 1h), so a long downtime catches up cleanly.
func aggregateHourlyCatchup() {
	var lastBucket int64
	_ = getDB().QueryRow(`SELECT COALESCE(MAX(bucket), 0) FROM stats_hourly`).Scan(&lastBucket)
	now := time.Now().Unix()
	currentHourStart := now - (now % 3600)
	target := currentHourStart - 3600 // most recent CLOSED hour
	if lastBucket == 0 {
		// First run — bootstrap from oldest request available.
		var minTS int64
		_ = getDB().QueryRow(`SELECT COALESCE(MIN(ts), 0) FROM requests`).Scan(&minTS)
		if minTS == 0 {
			return
		}
		lastBucket = minTS - (minTS % 3600) - 3600
	}
	for b := lastBucket + 3600; b <= target; b += 3600 {
		aggregateBucket(b, 3600, "stats_hourly")
	}
}

func aggregateDailyCatchup() {
	var lastBucket int64
	_ = getDB().QueryRow(`SELECT COALESCE(MAX(bucket), 0) FROM stats_daily`).Scan(&lastBucket)
	now := time.Now().Unix()
	currentDayStart := now - (now % 86400)
	target := currentDayStart - 86400
	if lastBucket == 0 {
		var minTS int64
		_ = getDB().QueryRow(`SELECT COALESCE(MIN(bucket), 0) FROM stats_hourly`).Scan(&minTS)
		if minTS == 0 {
			return
		}
		lastBucket = minTS - (minTS % 86400) - 86400
	}
	for b := lastBucket + 86400; b <= target; b += 86400 {
		aggregateBucket(b, 86400, "stats_daily")
	}
}

// aggregateBucket reads raw requests in [start, start+span) and groups by
// (model, proxy_id), writing one row per group.
// Source for hourly = requests; for daily = stats_hourly (already aggregated).
func aggregateBucket(start, span int64, target string) {
	end := start + span

	// We pre-aggregate latencies in Go to compute p50/p95 (SQLite percentile is awkward).
	type key struct {
		model string
		proxy int64
	}
	type acc struct {
		count, success, fail   int
		totalMs, promptT, outT int64
		latencies              []int
	}
	groups := map[key]*acc{}

	var rows interface {
		Close() error
		Next() bool
		Scan(...interface{}) error
	}
	var err error

	if target == "stats_hourly" {
		// from raw requests
		r, e := getDB().Query(`SELECT model, COALESCE(proxy_id,0), status, total_ms, prompt_tokens, output_tokens
			FROM requests WHERE ts >= ? AND ts < ?`, start, end)
		err = e
		rows = r
	} else {
		// from hourly aggregates
		r, e := getDB().Query(`SELECT model, proxy_id, requests, successes, failures, total_ms, prompt_tokens, output_tokens, p50_ms, p95_ms
			FROM stats_hourly WHERE bucket >= ? AND bucket < ?`, start, end)
		err = e
		rows = r
	}
	if err != nil {
		logf("[agg] %s query failed: %v", target, err)
		return
	}
	defer rows.Close()

	if target == "stats_hourly" {
		for rows.Next() {
			var model string
			var proxyID int64
			var status int
			var totalMs, pt, ot int64
			if err := rows.Scan(&model, &proxyID, &status, &totalMs, &pt, &ot); err != nil {
				continue
			}
			k := key{model, proxyID}
			a := groups[k]
			if a == nil {
				a = &acc{}
				groups[k] = a
			}
			a.count++
			if status == 200 {
				a.success++
			} else {
				a.fail++
			}
			a.totalMs += totalMs
			a.promptT += pt
			a.outT += ot
			a.latencies = append(a.latencies, int(totalMs))
		}
	} else {
		// daily: roll up hourly rows. p50/p95 = avg of hourly p50/p95 (good enough).
		type dailyAcc struct {
			acc
			p50Sum, p95Sum, p50N, p95N int
		}
		dgroups := map[key]*dailyAcc{}
		for rows.Next() {
			var model string
			var proxyID int64
			var requests, successes, failures int
			var totalMs, pt, ot int64
			var p50, p95 int
			if err := rows.Scan(&model, &proxyID, &requests, &successes, &failures, &totalMs, &pt, &ot, &p50, &p95); err != nil {
				continue
			}
			k := key{model, proxyID}
			a := dgroups[k]
			if a == nil {
				a = &dailyAcc{}
				dgroups[k] = a
			}
			a.count += requests
			a.success += successes
			a.fail += failures
			a.totalMs += totalMs
			a.promptT += pt
			a.outT += ot
			if p50 > 0 {
				a.p50Sum += p50
				a.p50N++
			}
			if p95 > 0 {
				a.p95Sum += p95
				a.p95N++
			}
		}
		tx, err := getDB().Begin()
		if err != nil {
			return
		}
		for k, a := range dgroups {
			p50 := 0
			if a.p50N > 0 {
				p50 = a.p50Sum / a.p50N
			}
			p95 := 0
			if a.p95N > 0 {
				p95 = a.p95Sum / a.p95N
			}
			_, _ = tx.Exec(upsert("stats_daily",
				[]string{"bucket", "model", "proxy_id", "requests", "successes", "failures",
					"total_ms", "p50_ms", "p95_ms", "prompt_tokens", "output_tokens"},
				[]string{"bucket", "model", "proxy_id"}),
				start, k.model, k.proxy,
				a.count, a.success, a.fail, a.totalMs, p50, p95, a.promptT, a.outT)
		}
		_ = tx.Commit()
		return
	}

	// hourly bucket commit
	tx, err := getDB().Begin()
	if err != nil {
		return
	}
	for k, a := range groups {
		p50, p95 := percentiles(a.latencies)
		_, _ = tx.Exec(upsert("stats_hourly",
			[]string{"bucket", "model", "proxy_id", "requests", "successes", "failures",
				"total_ms", "p50_ms", "p95_ms", "prompt_tokens", "output_tokens"},
			[]string{"bucket", "model", "proxy_id"}),
			start, k.model, k.proxy,
			a.count, a.success, a.fail, a.totalMs, p50, p95, a.promptT, a.outT)
	}
	_ = tx.Commit()
}

func percentiles(latencies []int) (p50, p95 int) {
	if len(latencies) == 0 {
		return 0, 0
	}
	sort.Ints(latencies)
	p50 = latencies[len(latencies)*50/100]
	p95idx := len(latencies) * 95 / 100
	if p95idx >= len(latencies) {
		p95idx = len(latencies) - 1
	}
	p95 = latencies[p95idx]
	return
}

// retentionSweep deletes raw requests older than RetentionDays and expired sessions.
func retentionSweep() {
	if rtCfg().RetentionDays <= 0 {
		return
	}
	cutoff := time.Now().Unix() - int64(rtCfg().RetentionDays)*86400
	res, err := getDB().Exec(`DELETE FROM requests WHERE ts < ?`, cutoff)
	if err != nil {
		logf("[retention] failed: %v", err)
		return
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		logf("[retention] purged %d old request rows", n)
	}
	_, _ = getDB().Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
}
