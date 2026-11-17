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

type healthResp struct {
	Status         string `json:"status"`
	TokenExpiresIn int    `json:"token_expires_in"`
	LastRefresh    string `json:"last_refresh"`
	LastError      string `json:"last_error"`
}

// getHealth 请求 /health，返回状态码、解析后的响应和原始 body。
func getHealth(t *testing.T, env *testEnv) (int, healthResp, string) {
	t.Helper()
	resp, err := http.Get(env.proxy.URL + "/health")
	require.NoError(t, err)
	body := readAll(t, resp)
	var h healthResp
	require.NoError(t, json.Unmarshal(body, &h), "health 响应不是 JSON：%q", body)
	return resp.StatusCode, h, string(body)
}

func TestHealthEndpoint(t *testing.T) {
	env := newTestEnv(t)

	status, h, _ := getHealth(t, env)
	assert.Equal(t, http.StatusServiceUnavailable, status, "before token: status")
	assert.Equal(t, "no_token", h.Status, "before token: status field")
	assert.Zero(t, h.TokenExpiresIn, "before token: token_expires_in")
	assert.Empty(t, h.LastRefresh, "before token: last_refresh")
	assert.Zero(t, env.f.tokenHits.Load(), "health triggered token fetch")

	_, _, err := env.m.Token(context.Background())
	require.NoError(t, err)
	status, h, body := getHealth(t, env)
	want := healthResp{Status: "ok", TokenExpiresIn: 7200, LastRefresh: env.clock.Now().Format(time.RFC3339)}
	assert.Equal(t, http.StatusOK, status, "after token: status")
	assert.Equal(t, want, h, "after token")
	assert.NotContains(t, body, "t-first", "health leaks token")
}

func TestHealthEndpoint_Idle(t *testing.T) {
	env := newTestEnv(t)
	_, _, err := env.m.Token(context.Background())
	require.NoError(t, err)
	refreshed := env.clock.Now().Format(time.RFC3339)

	env.clock.Advance(7200*time.Second - 4*time.Minute)
	status, h, _ := getHealth(t, env)
	assert.Equal(t, http.StatusOK, status, "idle: status")
	assert.Equal(t, healthResp{Status: "idle", TokenExpiresIn: 240, LastRefresh: refreshed}, h, "idle")

	env.f.setFailCode(10003)
	_, _, err = env.m.Token(context.Background())
	require.Error(t, err)
	status, h, _ = getHealth(t, env)
	assert.Equal(t, http.StatusServiceUnavailable, status, "after failure: status")
	assert.Equal(t, "no_token", h.Status, "after failure: status field")
	assert.Contains(t, h.LastError, "10003", "after failure: last_error")
	assert.EqualValues(t, 2, env.f.tokenHits.Load(), "tokenHits")
}

func TestHealthEndpoint_OkWithLastError(t *testing.T) {
	env := newTestEnv(t)
	_, _, err := env.m.Token(context.Background())
	require.NoError(t, err)
	env.f.setFailCode(10003)
	env.clock.Advance(7200*time.Second - 20*time.Minute)
	env.m.RefreshIfNeeded(context.Background())

	status, h, _ := getHealth(t, env)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "ok", h.Status)
	assert.Contains(t, h.LastError, "10003")
}
