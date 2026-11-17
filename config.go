package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	DefaultListen   = "127.0.0.1:18080"
	DefaultUpstream = "https://open.feishu.cn"
)

// Config 对应 config.json。错误信息只引用字段名和非敏感取值，AppSecret 的值始终不进入任何错误信息。
type Config struct {
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
	Listen    string `json:"listen"`
	Upstream  string `json:"upstream"`
	LogFile   string `json:"log_file"`
	// LazyRefresh 为 true 时不在后台定时刷新，token 只在请求到来时按需获取
	LazyRefresh bool `json:"lazy_refresh"`

	upstreamURL *url.URL
}

// LoadConfig 读取并校验配置文件：补默认值，要求 listen 为回环地址、upstream 为不带路径的 https 地址，
// 相对路径的 log_file 按配置文件所在目录解析。
func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败：%w", err)
	}
	defer f.Close()

	var c Config
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败：%w", path, err)
	}

	if c.AppID == "" {
		return nil, errors.New("配置项 app_id 不能为空")
	}
	if c.AppSecret == "" {
		return nil, errors.New("配置项 app_secret 不能为空")
	}

	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if err := checkListen(c.Listen); err != nil {
		return nil, err
	}

	if c.Upstream == "" {
		c.Upstream = DefaultUpstream
	}
	c.Upstream = strings.TrimRight(c.Upstream, "/")
	u, err := parseUpstream(c.Upstream)
	if err != nil {
		return nil, err
	}
	c.upstreamURL = u

	if c.LogFile != "" && !filepath.IsAbs(c.LogFile) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("解析配置文件路径失败：%w", err)
		}
		c.LogFile = filepath.Join(filepath.Dir(abs), c.LogFile)
	}
	return &c, nil
}

// UpstreamURL 返回解析后的飞书地址，形如 https://open.feishu.cn。
func (c *Config) UpstreamURL() *url.URL {
	return c.upstreamURL
}

func checkListen(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("配置项 listen 格式错误（应为 host:port）：%q", listen)
	}
	if isLoopbackHost(host) {
		return nil
	}
	return fmt.Errorf("配置项 listen 只能是回环地址（127.0.0.1、::1、localhost），当前为 %q", listen)
}

// isLoopbackHost 判断主机名是否为 localhost 或回环 IP（IPv6 可带方括号）。
func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func parseUpstream(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" || u.RawQuery != "" {
		return nil, fmt.Errorf("配置项 upstream 必须是不带路径的 https 地址，如 %s，当前为 %q", DefaultUpstream, raw)
	}
	return u, nil
}
