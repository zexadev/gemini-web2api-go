package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHandleAdminQuota_Cached(t *testing.T) {
	mockTime := time.Date(2026, 11, 22, 20, 0, 0, 0, time.UTC)
	oldNow := quotaNow
	quotaNow = func() time.Time { return mockTime }
	defer func() { quotaNow = oldNow }()

	acctID := addTestAccount(t, "admin_quota_acct")

	putCachedAccountQuota(acctID, AccountQuota{
		AccountID: acctID,
		Budget:    48384,
		Remaining: 39959,
		Ratio:     0.8259,
		Exhausted: false,
		FetchedAt: mockTime,
	})

	req := httptest.NewRequest("GET", "/admin/api/quota", nil)
	w := httptest.NewRecorder()

	handleAdminQuota(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp struct {
		Now      int64 `json:"now"`
		Accounts []struct {
			AccountID int64 `json:"account_id"`
			Status    string
			Quota     AccountQuota `json:"quota"`
		} `json:"accounts"`
	}

	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	found := false
	for _, a := range resp.Accounts {
		if a.AccountID == acctID {
			found = true
			if a.Status != "ok" {
				t.Errorf("expected status 'ok', got %s", a.Status)
			}
			if a.Quota.Budget != 48384 || a.Quota.Remaining != 39959 {
				t.Errorf("unexpected quota: %+v", a.Quota)
			}
		}
	}

	if !found {
		t.Errorf("account %d not found in quota response", acctID)
	}
}

func TestHandleAdminQuota_MethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest("POST", "/admin/api/quota", nil)
	w := httptest.NewRecorder()

	handleAdminQuota(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected status 405, got %d", w.Code)
	}
}
