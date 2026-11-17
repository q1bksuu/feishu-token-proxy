package main

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			require.NoError(t, err)
			body := readAll(t, resp)
			if !assert.Equal(t, http.StatusForbidden, resp.StatusCode, "%s %s", c.name, path) {
				continue
			}
			code, _ := proxyErr(t, body)
			assert.Equal(t, -1, code, "%s %s", c.name, path)
		}
	}
	assert.Zero(t, env.f.tokenHits.Load(), "rejected requests triggered token fetch")

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
		require.NoError(t, err)
		readAll(t, resp)
		assert.Equal(t, http.StatusOK, resp.StatusCode, c.name)
	}
}

func mustPort(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u.Port()
}
