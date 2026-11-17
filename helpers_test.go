package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAppID     = "cli_test"
	testAppSecret = "secret-test"
)

// fakeClock 是可手动推进的时钟，起点 2026-01-01T00:00:00Z。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// lockedBuffer 是并发安全的日志缓冲。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeFeishu 模拟飞书：tokenPath 返回 tokens 里的 token，其余请求交给 api。
type fakeFeishu struct {
	srv       *httptest.Server
	tokenHits atomic.Int32

	mu         sync.Mutex
	tokens     []string // 依次返回，用完后一直重复最后一个
	expire     int
	failCode   int // 非 0 时 token 接口返回 HTTP 200 + {"code":failCode,"msg":"fake failure"}
	tokenDelay time.Duration
	lastBody   map[string]string
	lastCT     string
	api        http.HandlerFunc
}

func newFakeFeishu(t *testing.T) *fakeFeishu {
	t.Helper()
	f := &fakeFeishu{tokens: []string{"t-first"}, expire: 7200}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeFeishu) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != tokenPath {
		f.mu.Lock()
		api := f.api
		f.mu.Unlock()
		if api == nil {
			http.NotFound(w, r)
			return
		}
		api(w, r)
		return
	}

	f.tokenHits.Add(1)
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)

	f.mu.Lock()
	f.lastBody = body
	f.lastCT = r.Header.Get("Content-Type")
	delay := f.tokenDelay
	failCode := f.failCode
	tok := f.tokens[0]
	if len(f.tokens) > 1 {
		f.tokens = f.tokens[1:]
	}
	expire := f.expire
	f.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if failCode != 0 {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": failCode, "msg": "fake failure"})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code": 0, "msg": "ok", "tenant_access_token": tok, "expire": expire,
	})
}

func (f *fakeFeishu) url() *url.URL {
	u, err := url.Parse(f.srv.URL)
	if err != nil {
		panic(err)
	}
	return u
}

func (f *fakeFeishu) setTokens(tokens ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = tokens
}

func (f *fakeFeishu) setFailCode(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCode = code
}

func (f *fakeFeishu) setTokenDelay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenDelay = d
}

func (f *fakeFeishu) setAPI(h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.api = h
}

func (f *fakeFeishu) snapshot() (body map[string]string, ct string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastBody, f.lastCT
}

// newTestManager 返回接到 f 上的 TokenManager，时钟用 fakeClock，日志写进返回的 buffer。
func newTestManager(t *testing.T, f *fakeFeishu) (*TokenManager, *fakeClock, *lockedBuffer) {
	t.Helper()
	logs := &lockedBuffer{}
	clock := newFakeClock()
	m := NewTokenManager(f.url(), testAppID, testAppSecret, f.srv.Client(), log.New(logs, "", 0))
	m.now = clock.Now
	return m, clock, logs
}

// testEnv 是一套完整的测试环境：假飞书、TokenManager、Server，以及指向 Server 的本地 HTTP 服务。
type testEnv struct {
	f     *fakeFeishu
	m     *TokenManager
	clock *fakeClock
	logs  *lockedBuffer
	s     *Server
	proxy *httptest.Server
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	f := newFakeFeishu(t)
	m, clock, logs := newTestManager(t, f)
	s := NewServer(f.url(), m, f.srv.Client().Transport, log.New(logs, "", 0))
	proxy := httptest.NewServer(s.Handler())
	t.Cleanup(proxy.Close)
	return &testEnv{f: f, m: m, clock: clock, logs: logs, s: s, proxy: proxy}
}

// waitForLog 等待日志出现 substr；请求日志在响应发完后才写入，需要短暂等待。
func waitForLog(t *testing.T, logs *lockedBuffer, substr string) {
	t.Helper()
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Contains(c, logs.String(), substr)
	}, 2*time.Second, 5*time.Millisecond)
}

func (f *fakeFeishu) setExpire(sec int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expire = sec
}

// advancingTransport 在每次请求前把假时钟推进 d，模拟耗时较长的获取。
type advancingTransport struct {
	base  http.RoundTripper
	clock *fakeClock
	d     time.Duration
}

func (a *advancingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	a.clock.Advance(a.d)
	return a.base.RoundTrip(req)
}
