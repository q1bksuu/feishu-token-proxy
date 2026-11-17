package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeConfig 把 body 写到 t.TempDir()/config.json，返回路径
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestLoadConfig_Defaults(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, `{"app_id":"cli_test","app_secret":"secret-test"}`))
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:18080", c.Listen, "Listen")
	assert.Equal(t, "https://open.feishu.cn", c.Upstream, "Upstream")
	assert.Empty(t, c.LogFile, "LogFile")
	assert.Equal(t, "https://open.feishu.cn", c.UpstreamURL().String(), "UpstreamURL")
}

func TestLoadConfig_LogFileRelativeToConfigDir(t *testing.T) {
	p := writeConfig(t, `{"app_id":"cli_test","app_secret":"secret-test","log_file":"proxy.log"}`)
	c, err := LoadConfig(p)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(filepath.Dir(p), "proxy.log"), c.LogFile)
}

func TestLoadConfig_Listen(t *testing.T) {
	ok := []string{"127.0.0.1:18080", "localhost:18080", "[::1]:18080"}
	bad := []string{"0.0.0.0:18080", ":18080", "192.168.1.10:18080", "127.0.0.1"}
	for _, l := range ok {
		body := `{"app_id":"cli_test","app_secret":"secret-test","listen":"` + l + `"}`
		c, err := LoadConfig(writeConfig(t, body))
		if !assert.NoError(t, err, "listen %q", l) {
			continue
		}
		assert.Equal(t, l, c.Listen, "listen %q", l)
	}
	for _, l := range bad {
		body := `{"app_id":"cli_test","app_secret":"secret-test","listen":"` + l + `"}`
		_, err := LoadConfig(writeConfig(t, body))
		assert.ErrorContains(t, err, "listen", "listen %q", l)
	}
}

func TestLoadConfig_Upstream(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, `{"app_id":"cli_test","app_secret":"secret-test","upstream":"https://open.larksuite.com/"}`))
	require.NoError(t, err)
	assert.Equal(t, "https://open.larksuite.com", c.Upstream, "Upstream")
	assert.Equal(t, "https://open.larksuite.com", c.UpstreamURL().String(), "UpstreamURL")

	for _, u := range []string{"http://open.feishu.cn", "https://open.feishu.cn/api", "open.feishu.cn"} {
		body := `{"app_id":"cli_test","app_secret":"secret-test","upstream":"` + u + `"}`
		_, err := LoadConfig(writeConfig(t, body))
		assert.ErrorContains(t, err, "upstream", "upstream %q", u)
	}
}

func TestLoadConfig_RequiresCredentials(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, `{"app_secret":"secret-test"}`))
	assert.ErrorContains(t, err, "app_id", "missing app_id")
	_, err = LoadConfig(writeConfig(t, `{"app_id":"cli_test"}`))
	assert.ErrorContains(t, err, "app_secret", "missing app_secret")
}

func TestLoadConfig_RejectsUnknownFields(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, `{"appid":"x","app_id":"cli_test","app_secret":"secret-test"}`))
	assert.ErrorContains(t, err, "appid")
}

func TestLoadConfig_ErrorsNeverContainSecret(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, `{"app_id":"cli_test","app_secret":"s3cr3t-xyz","listen":"0.0.0.0:1"}`))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "s3cr3t-xyz", "error leaks secret")
}
