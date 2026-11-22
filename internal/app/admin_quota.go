package app

import (
	"context"
	"net/http"
	"time"
)

// handleAdminQuota handles GET /admin/api/quota.
// By default, it returns cached quota information to protect upstream quotas.
// When ?refresh=true is provided, it triggers an upstream probe (qpEbW) for enabled accounts.
func handleAdminQuota(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	forceRefresh := r.URL.Query().Get("refresh") == "true"
	var out []map[string]any

	for _, a := range accountList() {
		if a.Status != "enabled" {
			continue
		}

		item := map[string]any{
			"account_id": a.ID,
			"label":      accountDisplayName(&a),
			"authuser":   a.AuthUser,
			"proxy_id":   a.ProxyID,
		}

		if forceRefresh {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			q, err := fetchAccountQuotaUpstream(ctx, a.ID)
			cancel()
			if err != nil {
				item["status"] = "error"
				item["error"] = err.Error()
			} else {
				putCachedAccountQuota(a.ID, q)
				item["status"] = "ok"
				item["quota"] = q
			}
		} else {
			if q, ok := getCachedAccountQuota(a.ID); ok {
				item["status"] = "ok"
				item["quota"] = q
			} else {
				item["status"] = "uncached"
			}
		}

		out = append(out, item)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"now":      time.Now().Unix(),
		"accounts": out,
	})
}
