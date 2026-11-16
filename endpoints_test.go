package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
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
		if err != nil {
			t.Fatal(err)
		}
		var r tokenResp
		if err := json.Unmarshal(readAll(t, resp), &r); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, r
	}

	status, r := get()
	want := tokenResp{Code: 0, Msg: "ok", TenantAccessToken: "t-first", Expire: 7200}
	if status != http.StatusOK || r != want {
		t.Errorf("GET /token = %d %+v, want 200 %+v", status, r, want)
	}

	env.clock.Advance(100 * time.Second)
	if _, r = get(); r.Expire != 7100 {
		t.Errorf("expire after 100s = %d, want 7100", r.Expire)
	}

	resp, err := http.Post(env.proxy.URL+"/token", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, resp)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /token = %d, want 405", resp.StatusCode)
	}
}

func TestTokenEndpoint_Failure(t *testing.T) {
	env := newTestEnv(t)
	env.f.setFailCode(10003)

	resp, err := http.Get(env.proxy.URL + "/token")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if code, msg := proxyErr(t, body); code != -1 || !strings.Contains(msg, "10003") {
		t.Errorf("error = %d %q", code, msg)
	}
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
		if err != nil {
			t.Fatal(err)
		}
		body := readAll(t, resp)
		var h healthResp
		if err := json.Unmarshal(body, &h); err != nil {
			t.Fatalf("health 响应不是 JSON：%q", body)
		}
		return resp.StatusCode, h, string(body)
	}

	status, h, _ := get()
	if status != http.StatusServiceUnavailable || h.Status != "no_token" || h.TokenExpiresIn != 0 || h.LastRefresh != "" {
		t.Errorf("before token: %d %+v", status, h)
	}
	if n := env.f.tokenHits.Load(); n != 0 {
		t.Errorf("health triggered token fetch: tokenHits = %d", n)
	}

	if _, _, err := env.m.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, h, body := get()
	want := healthResp{Status: "ok", TokenExpiresIn: 7200, LastRefresh: env.clock.Now().Format(time.RFC3339)}
	if status != http.StatusOK || h != want {
		t.Errorf("after token: %d %+v, want 200 %+v", status, h, want)
	}
	if strings.Contains(body, "t-first") {
		t.Errorf("health leaks token: %s", body)
	}
}
