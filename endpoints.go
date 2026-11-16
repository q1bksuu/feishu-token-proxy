package main

import (
	"net/http"
	"time"
)

// handleToken 返回当前可用的 token，格式与飞书获取 tenant_access_token 接口一致，expire 为剩余秒数。
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	tok, expiresAt, err := s.tokens.Token(r.Context())
	if err != nil {
		writeProxyError(w, http.StatusBadGateway, "获取 tenant_access_token 失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}{0, "ok", tok, int(expiresAt.Sub(s.tokens.now()).Seconds())})
}

// handleHealth 只读取 TokenManager 的状态，token 可用时返回 200，否则 503。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	st := s.tokens.Status()
	resp := struct {
		Status         string `json:"status"`
		TokenExpiresIn int    `json:"token_expires_in"`
		LastRefresh    string `json:"last_refresh"`
		LastError      string `json:"last_error"`
	}{
		Status:         "ok",
		TokenExpiresIn: int(st.ExpiresIn.Seconds()),
		LastError:      st.LastError,
	}
	if !st.LastRefresh.IsZero() {
		resp.LastRefresh = st.LastRefresh.Format(time.RFC3339)
	}
	status := http.StatusOK
	if !st.Valid {
		resp.Status = "no_token"
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, resp)
}
