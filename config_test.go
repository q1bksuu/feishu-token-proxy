package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig 把 body 写到 t.TempDir()/config.json，返回路径
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfig_Defaults(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, `{"app_id":"cli_test","app_secret":"secret-test"}`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c.Listen != "127.0.0.1:18080" {
		t.Errorf("Listen = %q", c.Listen)
	}
	if c.Upstream != "https://open.feishu.cn" {
		t.Errorf("Upstream = %q", c.Upstream)
	}
	if c.LogFile != "" {
		t.Errorf("LogFile = %q", c.LogFile)
	}
	if got := c.UpstreamURL().String(); got != "https://open.feishu.cn" {
		t.Errorf("UpstreamURL = %q", got)
	}
}

func TestLoadConfig_LogFileRelativeToConfigDir(t *testing.T) {
	p := writeConfig(t, `{"app_id":"cli_test","app_secret":"secret-test","log_file":"proxy.log"}`)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if want := filepath.Join(filepath.Dir(p), "proxy.log"); c.LogFile != want {
		t.Errorf("LogFile = %q, want %q", c.LogFile, want)
	}
}

func TestLoadConfig_Listen(t *testing.T) {
	ok := []string{"127.0.0.1:18080", "localhost:18080", "[::1]:18080"}
	bad := []string{"0.0.0.0:18080", ":18080", "192.168.1.10:18080", "127.0.0.1"}
	for _, l := range ok {
		body := `{"app_id":"cli_test","app_secret":"secret-test","listen":"` + l + `"}`
		c, err := LoadConfig(writeConfig(t, body))
		if err != nil {
			t.Errorf("listen %q: unexpected error %v", l, err)
			continue
		}
		if c.Listen != l {
			t.Errorf("listen %q: got %q", l, c.Listen)
		}
	}
	for _, l := range bad {
		body := `{"app_id":"cli_test","app_secret":"secret-test","listen":"` + l + `"}`
		_, err := LoadConfig(writeConfig(t, body))
		if err == nil || !strings.Contains(err.Error(), "listen") {
			t.Errorf("listen %q: err = %v, want error mentioning listen", l, err)
		}
	}
}

func TestLoadConfig_Upstream(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, `{"app_id":"cli_test","app_secret":"secret-test","upstream":"https://open.larksuite.com/"}`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c.Upstream != "https://open.larksuite.com" {
		t.Errorf("Upstream = %q", c.Upstream)
	}
	if got := c.UpstreamURL().String(); got != "https://open.larksuite.com" {
		t.Errorf("UpstreamURL = %q", got)
	}

	for _, u := range []string{"http://open.feishu.cn", "https://open.feishu.cn/api", "open.feishu.cn"} {
		body := `{"app_id":"cli_test","app_secret":"secret-test","upstream":"` + u + `"}`
		_, err := LoadConfig(writeConfig(t, body))
		if err == nil || !strings.Contains(err.Error(), "upstream") {
			t.Errorf("upstream %q: err = %v, want error mentioning upstream", u, err)
		}
	}
}

func TestLoadConfig_RequiresCredentials(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, `{"app_secret":"secret-test"}`))
	if err == nil || !strings.Contains(err.Error(), "app_id") {
		t.Errorf("missing app_id: err = %v", err)
	}
	_, err = LoadConfig(writeConfig(t, `{"app_id":"cli_test"}`))
	if err == nil || !strings.Contains(err.Error(), "app_secret") {
		t.Errorf("missing app_secret: err = %v", err)
	}
}

func TestLoadConfig_RejectsUnknownFields(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, `{"appid":"x","app_id":"cli_test","app_secret":"secret-test"}`))
	if err == nil || !strings.Contains(err.Error(), "appid") {
		t.Errorf("err = %v, want error mentioning appid", err)
	}
}

func TestLoadConfig_ErrorsNeverContainSecret(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, `{"app_id":"cli_test","app_secret":"s3cr3t-xyz","listen":"0.0.0.0:1"}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "s3cr3t-xyz") {
		t.Errorf("error leaks secret: %v", err)
	}
}
