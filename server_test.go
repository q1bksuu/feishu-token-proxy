package main

import (
	"net/http"
	"net/url"
	"testing"
)

func TestServer_RejectsNonLocalHostAndBrowserRequests(t *testing.T) {
	env := newTestEnv(t)
	proxyPort := mustPort(t, env.proxy.URL)

	rejected := []struct {
		name   string
		host   string
		header map[string]string
	}{
		{"外部域名 Host", "attacker.example:" + proxyPort, nil},
		{"带 Origin", "", map[string]string{"Origin": "http://evil.example"}},
		{"跨站 Sec-Fetch-Site", "", map[string]string{"Sec-Fetch-Site": "cross-site"}},
	}
	for _, c := range rejected {
		for _, path := range []string{"/token", "/open-apis/im/v1/chats"} {
			req, _ := http.NewRequest(http.MethodGet, env.proxy.URL+path, nil)
			if c.host != "" {
				req.Host = c.host
			}
			for k, v := range c.header {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body := readAll(t, resp)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s: status = %d, want 403", c.name, path, resp.StatusCode)
				continue
			}
			if code, _ := proxyErr(t, body); code != -1 {
				t.Errorf("%s %s: code = %d, want -1", c.name, path, code)
			}
		}
	}
	if n := env.f.tokenHits.Load(); n != 0 {
		t.Errorf("rejected requests triggered token fetch: tokenHits = %d", n)
	}

	allowed := []struct {
		name   string
		host   string
		header map[string]string
	}{
		{"localhost", "localhost:" + proxyPort, nil},
		{"::1", "[::1]:" + proxyPort, nil},
		{"Sec-Fetch-Site none", "", map[string]string{"Sec-Fetch-Site": "none"}},
	}
	for _, c := range allowed {
		req, _ := http.NewRequest(http.MethodGet, env.proxy.URL+"/token", nil)
		if c.host != "" {
			req.Host = c.host
		}
		for k, v := range c.header {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		readAll(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", c.name, resp.StatusCode)
		}
	}
}

func mustPort(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}
