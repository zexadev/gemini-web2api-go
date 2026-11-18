package app

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// AccountUnavailableError 表示这次挑中的账号因为出口状态不能用。
//
// 跟 ModelDowngradedError 分开：那是“模型额度被降级”，这是“账号/出口暂时不可用”。
// 调用方（streamGenerateWithFiles）看到 AccountID>0 且还没吐内容时，可以换下一个
// 账号重试；全部都不行再把最后一个错误返回给客户端。
type AccountUnavailableError struct {
	// Reason: proxy_cooling | proxy_full | proxy_disabled | proxy_deleted |
	// proxy_conflict | account_disabled
	Reason       string
	AccountID    int64
	AccountLabel string
	ProxyID      int64
	ProxyName    string
	RetryAfter   time.Duration
	// Detail 非空时直接作为错误信息（用于“没有具体账号”的池级错误）。
	Detail string
	// PoolExhausted 为真表示挑号阶段已经确认没有别的可用账号（不是单个号失败），
	// 外层不必再换号重试。
	PoolExhausted bool
}

func (e *AccountUnavailableError) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	acct := e.AccountLabel
	if acct == "" && e.AccountID > 0 {
		acct = fmt.Sprintf("#%d", e.AccountID)
	}
	proxy := e.ProxyName
	if proxy == "" && e.ProxyID > 0 {
		proxy = fmt.Sprintf("#%d", e.ProxyID)
	}
	who := "account"
	if acct != "" {
		who = "account " + acct
	}
	switch e.Reason {
	case "proxy_cooling":
		if e.RetryAfter > 0 {
			return fmt.Sprintf("%s's exclusive proxy %s is cooling down; retry after %s",
				who, proxy, e.RetryAfter.Round(time.Second))
		}
		return fmt.Sprintf("%s's exclusive proxy %s is cooling down", who, proxy)
	case "proxy_full":
		if e.RetryAfter > 0 {
			return fmt.Sprintf("%s's exclusive proxy %s is at its IP limit; retry after %s",
				who, proxy, e.RetryAfter.Round(time.Second))
		}
		return fmt.Sprintf("%s's exclusive proxy %s is at its IP limit", who, proxy)
	case "proxy_disabled":
		return fmt.Sprintf("%s's exclusive proxy %s is disabled", who, proxy)
	case "proxy_deleted":
		return fmt.Sprintf("%s's exclusive proxy %s was deleted", who, proxy)
	case "proxy_conflict":
		return fmt.Sprintf("%s has conflicting exclusive proxy bindings", who)
	case "account_disabled":
		return fmt.Sprintf("%s is disabled", who)
	default:
		return fmt.Sprintf("%s is unavailable (%s)", who, e.Reason)
	}
}

// HTTPStatus 把出口状态映射成对客户端的 HTTP 状态。
func (e *AccountUnavailableError) HTTPStatus() int {
	switch e.Reason {
	case "proxy_cooling", "proxy_full":
		return http.StatusTooManyRequests
	case "proxy_disabled", "proxy_deleted", "proxy_conflict":
		return http.StatusBadGateway
	default:
		return http.StatusServiceUnavailable
	}
}

// Code 是 OpenAI 风格错误体里的 code。
func (e *AccountUnavailableError) Code() string {
	switch e.Reason {
	case "proxy_cooling", "proxy_full":
		return "proxy_cooling"
	case "proxy_disabled", "proxy_deleted", "proxy_conflict":
		return "proxy_unavailable"
	default:
		return "account_unavailable"
	}
}

// RetryableWithAnotherAccount 表示这次失败只跟选中的账号/出口有关，换号可能成功。
func (e *AccountUnavailableError) RetryableWithAnotherAccount() bool {
	return e.AccountID > 0 && e.Reason != "account_disabled" && !e.PoolExhausted
}

// KeyAccountForbiddenError 表示 API key 的账号白名单不允许这次请求。
type KeyAccountForbiddenError struct {
	KeyName string
	Model   string
	Detail  string
}

func (e *KeyAccountForbiddenError) Error() string {
	who := e.KeyName
	if who == "" {
		who = "this API key"
	}
	if e.Detail != "" {
		return fmt.Sprintf("%s cannot use this account/model: %s", who, e.Detail)
	}
	if e.Model != "" {
		return fmt.Sprintf("%s has no allowed Google account for model %s", who, e.Model)
	}
	return fmt.Sprintf("%s has no allowed Google account for this request", who)
}

func (e *KeyAccountForbiddenError) HTTPStatus() int { return http.StatusForbidden }
func (e *KeyAccountForbiddenError) Code() string    { return "key_account_forbidden" }

// apiAccountErrorStatus 返回账号/Key 限制错误对应的 HTTP 状态。
// 第二个返回值为 false 表示不是这类错误。
func apiAccountErrorStatus(err error) (int, bool) {
	var aue *AccountUnavailableError
	if errors.As(err, &aue) {
		return aue.HTTPStatus(), true
	}
	var kfe *KeyAccountForbiddenError
	if errors.As(err, &kfe) {
		return kfe.HTTPStatus(), true
	}
	return 0, false
}

// apiAccountErrorCode 返回账号/Key 限制错误的 code；不是这类错误返回空串。
func apiAccountErrorCode(err error) string {
	var aue *AccountUnavailableError
	if errors.As(err, &aue) {
		return aue.Code()
	}
	var kfe *KeyAccountForbiddenError
	if errors.As(err, &kfe) {
		return kfe.Code()
	}
	return ""
}

// writeAPIAccountError 把账号/Key 限制错误写成 OpenAI 风格的 JSON。
// 返回 true 表示已经写过响应，调用方不要继续。
func writeAPIAccountError(w http.ResponseWriter, err error) bool {
	switch e := err.(type) {
	case *KeyAccountForbiddenError:
		writeJSON(w, e.HTTPStatus(), map[string]interface{}{"error": map[string]string{
			"message": e.Error(),
			"type":    "invalid_request_error",
			"code":    e.Code(),
		}})
		return true
	case *AccountUnavailableError:
		status := e.HTTPStatus()
		typ := "server_error"
		if status == http.StatusTooManyRequests {
			typ = "rate_limit_error"
		}
		if e.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int((e.RetryAfter+time.Second-1)/time.Second)))
		}
		writeJSON(w, status, map[string]interface{}{"error": map[string]string{
			"message": e.Error(),
			"type":    typ,
			"code":    e.Code(),
		}})
		return true
	}
	return false
}
