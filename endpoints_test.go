package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenEndpoint(t *testing.T) {
	env := newTestEnv(t)

	type tokenResp struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	get := func() (int, tokenResp) {
		t.Helper()
		resp, err := http.Get(env.proxy.URL + "/token")
		require.NoError(t, err)
		var r tokenResp
		require.NoError(t, json.Unmarshal(readAll(t, resp), &r))
		return resp.StatusCode, r
	}

	status, r := get()
	want := tokenResp{Code: 0, Msg: "ok", TenantAccessToken: "t-first", Expire: 7200}
	assert.Equal(t, http.StatusOK, status, "GET /token status")
	assert.Equal(t, want, r, "GET /token")

	env.clock.Advance(100 * time.Second)
	_, r = get()
	assert.Equal(t, 7100, r.Expire, "expire after 100s")

	resp, err := http.Post(env.proxy.URL+"/token", "application/json", nil)
	require.NoError(t, err)
	readAll(t, resp)
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, "POST /token")
}

func TestTokenEndpoint_Failure(t *testing.T) {
	env := newTestEnv(t)
	env.f.setFailCode(10003)

	resp, err := http.Get(env.proxy.URL + "/token")
	require.NoError(t, err)
	body := readAll(t, resp)
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	code, msg := proxyErr(t, body)
	assert.Equal(t, -1, code)
	assert.Contains(t, msg, "10003")
}

func TestHealthEndpoint(t *testing.T) {
	env := newTestEnv(t)

	type healthResp struct {
		Status         string `json:"status"`
		TokenExpiresIn int    `json:"token_expires_in"`
		LastRefresh    string `json:"last_refresh"`
		LastError      string `json:"last_error"`
	}
	get := func() (int, healthResp, string) {
		t.Helper()
		resp, err := http.Get(env.proxy.URL + "/health")
		require.NoError(t, err)
		body := readAll(t, resp)
		var h healthResp
		require.NoError(t, json.Unmarshal(body, &h), "health 响应不是 JSON：%q", body)
		return resp.StatusCode, h, string(body)
	}

	status, h, _ := get()
	assert.Equal(t, http.StatusServiceUnavailable, status, "before token: status")
	assert.Equal(t, "no_token", h.Status, "before token: status field")
	assert.Zero(t, h.TokenExpiresIn, "before token: token_expires_in")
	assert.Empty(t, h.LastRefresh, "before token: last_refresh")
	assert.Zero(t, env.f.tokenHits.Load(), "health triggered token fetch")

	_, _, err := env.m.Token(context.Background())
	require.NoError(t, err)
	status, h, body := get()
	want := healthResp{Status: "ok", TokenExpiresIn: 7200, LastRefresh: env.clock.Now().Format(time.RFC3339)}
	assert.Equal(t, http.StatusOK, status, "after token: status")
	assert.Equal(t, want, h, "after token")
	assert.NotContains(t, body, "t-first", "health leaks token")
}
