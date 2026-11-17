package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// apiCall 是假飞书 api 收到的一次请求。
type apiCall struct {
	method   string
	path     string
	rawQuery string
	header   http.Header
	body     []byte
}

type apiRecorder struct {
	mu    sync.Mutex
	calls []apiCall
}

func (r *apiRecorder) record(req *http.Request) apiCall {
	body, _ := io.ReadAll(req.Body)
	c := apiCall{
		method:   req.Method,
		path:     req.URL.Path,
		rawQuery: req.URL.RawQuery,
		header:   req.Header.Clone(),
		body:     body,
	}
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
	return c
}

func (r *apiRecorder) all() []apiCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]apiCall(nil), r.calls...)
}

// readAll 读完响应并关闭。
func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "读取响应失败")
	return b
}

// proxyErr 解析代理自身的错误 JSON。
func proxyErr(t *testing.T, body []byte) (int, string) {
	t.Helper()
	var e struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	require.NoError(t, json.Unmarshal(body, &e), "响应不是 JSON：%q", body)
	return e.Code, e.Msg
}

// invalidTokenAPI 对 t-first 回 400 + 99991663，对其他 token 回 200 并回显 body；gzip 为 true 时按请求的 Accept-Encoding 压缩失效响应。
func invalidTokenAPI(rec *apiRecorder, gz bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := rec.record(r)
		if r.Header.Get("Authorization") != "Bearer t-first" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(c.body)
			return
		}
		payload := []byte(`{"code":99991663,"msg":"invalid"}`)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if gz && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			_, _ = zw.Write(payload)
			_ = zw.Close()
			payload = buf.Bytes()
			w.Header().Set("Content-Encoding", "gzip")
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(payload)
	}
}

func TestForward_InjectsTokenAndStripsHeaders(t *testing.T) {
	env := newTestEnv(t)
	rec := &apiRecorder{}
	env.f.setAPI(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	})

	req, _ := http.NewRequest(http.MethodGet, env.proxy.URL+"/open-apis/im/v1/chats", nil)
	req.Header.Set("Authorization", "Bearer caller-junk")
	req.Header.Set("Connection", "X-Drop")
	req.Header.Set("X-Drop", "1")
	req.Header.Set("X-Keep", "1")
	req.Header.Set("Accept-Encoding", "br")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body := readAll(t, resp)

	calls := rec.all()
	require.Len(t, calls, 1, "api calls")
	h := calls[0].header
	assert.Equal(t, "Bearer t-first", h.Get("Authorization"), "upstream Authorization")
	assert.Equal(t, "1", h.Get("X-Keep"), "upstream X-Keep")
	assert.Empty(t, h.Get("X-Drop"), "upstream X-Drop, want hop-by-hop removed")
	assert.NotEqual(t, "br", h.Get("Accept-Encoding"), "upstream Accept-Encoding, want caller value removed")

	assert.Equal(t, http.StatusCreated, resp.StatusCode, "caller status")
	assert.Equal(t, "yes", resp.Header.Get("X-Upstream"), "caller X-Upstream")
	assert.Equal(t, "ok", string(body), "caller body")
	waitForLog(t, env.logs, "GET /open-apis/im/v1/chats 201")
	assert.NotContains(t, env.logs.String(), "t-first", "logs leak token")
}

func TestForward_PreservesMethodPathQueryBody(t *testing.T) {
	env := newTestEnv(t)
	rec := &apiRecorder{}
	env.f.setAPI(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusOK)
	})

	const rawQuery = "receive_id_type=chat_id&page_token=AQD9%2Bx%2Fy%3D%3D"
	const jsonBody = `{"receive_id":"oc_1","msg_type":"text"}`
	req, _ := http.NewRequest(http.MethodPost, env.proxy.URL+"/open-apis/im/v1/messages?"+rawQuery, strings.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	readAll(t, resp)

	var bin []byte
	for i := range 256 {
		bin = append(bin, byte(i))
	}
	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	fw, _ := mw.CreateFormFile("file", "bin.dat")
	_, _ = fw.Write(bin)
	_ = mw.WriteField("file_type", "stream")
	_ = mw.Close()
	multipartBody := append([]byte(nil), mp.Bytes()...)
	req, _ = http.NewRequest(http.MethodPut, env.proxy.URL+"/open-apis/im/v1/files", bytes.NewReader(multipartBody))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	readAll(t, resp)

	calls := rec.all()
	require.Len(t, calls, 2, "api calls")
	c := calls[0]
	assert.Equal(t, http.MethodPost, c.method, "json method")
	assert.Equal(t, "/open-apis/im/v1/messages", c.path, "json path")
	assert.Equal(t, rawQuery, c.rawQuery, "json rawQuery")
	assert.Equal(t, jsonBody, string(c.body), "json body")
	assert.Equal(t, "application/json; charset=utf-8", c.header.Get("Content-Type"), "json Content-Type")
	c = calls[1]
	assert.Equal(t, http.MethodPut, c.method, "multipart method")
	assert.Equal(t, multipartBody, c.body, "multipart body")
	assert.Equal(t, mw.FormDataContentType(), c.header.Get("Content-Type"), "multipart Content-Type")
}

func TestForward_RejectsUnknownPath(t *testing.T) {
	env := newTestEnv(t)
	rec := &apiRecorder{}
	env.f.setAPI(func(w http.ResponseWriter, r *http.Request) { rec.record(r) })

	resp, err := http.Get(env.proxy.URL + "/foo")
	require.NoError(t, err)
	body := readAll(t, resp)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	code, _ := proxyErr(t, body)
	assert.Equal(t, -1, code)
	assert.Empty(t, rec.all(), "api calls")
}

func TestForward_BodyTooLarge(t *testing.T) {
	env := newTestEnv(t)
	env.s.maxBody = 10
	rec := &apiRecorder{}
	env.f.setAPI(func(w http.ResponseWriter, r *http.Request) { rec.record(r) })

	resp, err := http.Post(env.proxy.URL+"/open-apis/im/v1/messages", "text/plain", strings.NewReader("01234567890"))
	require.NoError(t, err)
	body := readAll(t, resp)
	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	code, _ := proxyErr(t, body)
	assert.Equal(t, -1, code)
	assert.Empty(t, rec.all(), "api calls")
}

func TestForward_RetriesOnceOnInvalidToken(t *testing.T) {
	env := newTestEnv(t)
	env.f.setTokens("t-first", "t-second")
	rec := &apiRecorder{}
	env.f.setAPI(invalidTokenAPI(rec, false))
	_, _, err := env.m.Token(context.Background())
	require.NoError(t, err)
	env.clock.Advance(61 * time.Second)

	resp, err := http.Post(env.proxy.URL+"/open-apis/im/v1/messages", "text/plain", strings.NewReader("hello"))
	require.NoError(t, err)
	body := readAll(t, resp)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "caller status")
	assert.Equal(t, "hello", string(body), "caller body")
	calls := rec.all()
	require.Len(t, calls, 2, "api calls")
	assert.Equal(t, "Bearer t-second", calls[1].header.Get("Authorization"), "retry Authorization")
	assert.Equal(t, "hello", string(calls[1].body), "retry body")
	waitForLog(t, env.logs, "retried=true")
}

func TestForward_NoRetryWithinCooldown(t *testing.T) {
	env := newTestEnv(t)
	env.f.setTokens("t-first", "t-second")
	rec := &apiRecorder{}
	env.f.setAPI(invalidTokenAPI(rec, false))
	_, _, err := env.m.Token(context.Background())
	require.NoError(t, err)

	resp, err := http.Post(env.proxy.URL+"/open-apis/im/v1/messages", "text/plain", strings.NewReader("hello"))
	require.NoError(t, err)
	body := readAll(t, resp)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "caller status")
	assert.Contains(t, string(body), "99991663", "caller body")
	assert.Len(t, rec.all(), 1, "api calls")
	assert.EqualValues(t, 1, env.f.tokenHits.Load(), "tokenHits")
}

func TestForward_RetryWorksWhenCallerAcceptsGzip(t *testing.T) {
	env := newTestEnv(t)
	env.f.setTokens("t-first", "t-second")
	rec := &apiRecorder{}
	env.f.setAPI(invalidTokenAPI(rec, true))
	_, _, err := env.m.Token(context.Background())
	require.NoError(t, err)
	env.clock.Advance(61 * time.Second)

	req, _ := http.NewRequest(http.MethodPost, env.proxy.URL+"/open-apis/im/v1/messages", strings.NewReader("hello"))
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body := readAll(t, resp)
	assert.Len(t, rec.all(), 2, "api calls")
	assert.Equal(t, http.StatusOK, resp.StatusCode, "caller status")
	assert.Equal(t, "hello", string(body), "caller body")
	assert.Empty(t, resp.Header.Get("Content-Encoding"), "Content-Encoding")
}

func TestForward_PassesThroughOtherErrors(t *testing.T) {
	env := newTestEnv(t)
	const errBody = `{"code":99991672,"msg":"no scope"}`
	env.f.setAPI(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(errBody))
	})

	resp, err := http.Get(env.proxy.URL + "/open-apis/im/v1/chats")
	require.NoError(t, err)
	body := readAll(t, resp)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "caller status")
	assert.Equal(t, errBody, string(body), "caller body")
	assert.EqualValues(t, 1, env.f.tokenHits.Load(), "tokenHits")
}

func TestForward_PassesThroughLargeAndNonJSONErrors(t *testing.T) {
	t.Run("超大 JSON 错误体", func(t *testing.T) {
		env := newTestEnv(t)
		large := []byte(`{"code":99991663,"pad":"` + strings.Repeat("x", 2<<20) + `"}`)
		rec := &apiRecorder{}
		env.f.setAPI(func(w http.ResponseWriter, r *http.Request) {
			rec.record(r)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write(large)
		})
		_, _, err := env.m.Token(context.Background())
		require.NoError(t, err)
		env.clock.Advance(61 * time.Second)

		resp, err := http.Get(env.proxy.URL + "/open-apis/im/v1/chats")
		require.NoError(t, err)
		body := readAll(t, resp)
		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		assert.Equal(t, sha256.Sum256(large), sha256.Sum256(body), "body hash: got %d bytes, want %d", len(body), len(large))
		assert.Len(t, rec.all(), 1, "api calls")
	})

	t.Run("非 JSON 错误页", func(t *testing.T) {
		env := newTestEnv(t)
		const page = "<html>bad gateway</html>"
		env.f.setAPI(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(page))
		})
		resp, err := http.Get(env.proxy.URL + "/open-apis/im/v1/chats")
		require.NoError(t, err)
		body := readAll(t, resp)
		assert.Equal(t, http.StatusBadGateway, resp.StatusCode, "caller status")
		assert.Equal(t, page, string(body), "caller body")
		assert.Equal(t, "text/html", resp.Header.Get("Content-Type"), "caller Content-Type")
	})
}

func TestForward_StreamsLargeResponse(t *testing.T) {
	env := newTestEnv(t)
	head := bytes.Repeat([]byte("h"), 64<<10)
	tail := bytes.Repeat([]byte("t"), 8<<20)
	release := make(chan struct{})
	var once sync.Once
	closeRelease := func() { once.Do(func() { close(release) }) }
	t.Cleanup(closeRelease)
	env.f.setAPI(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(head)
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write(tail)
	})

	resp, err := http.Get(env.proxy.URL + "/open-apis/drive/v1/files/x/download")
	require.NoError(t, err)
	defer resp.Body.Close()

	gotHead := make(chan []byte, 1)
	go func() {
		buf := make([]byte, len(head))
		if _, err := io.ReadFull(resp.Body, buf); err != nil {
			gotHead <- nil
			return
		}
		gotHead <- buf
	}()
	var first []byte
	select {
	case first = <-gotHead:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "响应未流式透传")
	}
	closeRelease()
	rest, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	got := append(first, rest...)
	want := append(append([]byte(nil), head...), tail...)
	assert.Equal(t, len(want), len(got), "body length")
	assert.Equal(t, sha256.Sum256(want), sha256.Sum256(got), "body hash: got %d bytes, want %d", len(got), len(want))
}

func TestForward_TokenUnavailable(t *testing.T) {
	env := newTestEnv(t)
	env.f.setFailCode(10003)

	resp, err := http.Get(env.proxy.URL + "/open-apis/im/v1/chats")
	require.NoError(t, err)
	body := readAll(t, resp)
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	code, msg := proxyErr(t, body)
	assert.Equal(t, -1, code)
	assert.Contains(t, msg, "tenant_access_token")
	waitForLog(t, env.logs, "GET /open-apis/im/v1/chats 502")
	assert.NotContains(t, string(body), testAppSecret, "secret leaked in body")
	assert.NotContains(t, env.logs.String(), testAppSecret, "secret leaked in logs")
}

func TestForward_UpstreamUnreachable(t *testing.T) {
	env := newTestEnv(t)
	_, _, err := env.m.Token(context.Background())
	require.NoError(t, err)
	env.f.srv.Close()

	resp, err := http.Get(env.proxy.URL + "/open-apis/im/v1/chats")
	require.NoError(t, err)
	body := readAll(t, resp)
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	code, _ := proxyErr(t, body)
	assert.Equal(t, -1, code)
}

func TestForward_PreservesRawQueryVerbatim(t *testing.T) {
	env := newTestEnv(t)
	rec := &apiRecorder{}
	env.f.setAPI(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusOK)
	})

	const rawQuery = "query=a;b&discount=100%&page_size=10"
	resp, err := http.Get(env.proxy.URL + "/open-apis/search/v1/x?" + rawQuery)
	require.NoError(t, err)
	readAll(t, resp)
	calls := rec.all()
	require.Len(t, calls, 1, "api calls")
	assert.Equal(t, rawQuery, calls[0].rawQuery)
}
