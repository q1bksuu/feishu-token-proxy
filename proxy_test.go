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
	if err != nil {
		t.Fatalf("读取响应失败：%v", err)
	}
	return b
}

// proxyErr 解析代理自身的错误 JSON。
func proxyErr(t *testing.T, body []byte) (int, string) {
	t.Helper()
	var e struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("响应不是 JSON：%q", body)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)

	calls := rec.all()
	if len(calls) != 1 {
		t.Fatalf("api calls = %d, want 1", len(calls))
	}
	h := calls[0].header
	if got := h.Get("Authorization"); got != "Bearer t-first" {
		t.Errorf("upstream Authorization = %q", got)
	}
	if h.Get("X-Keep") != "1" {
		t.Errorf("upstream X-Keep = %q", h.Get("X-Keep"))
	}
	if h.Get("X-Drop") != "" {
		t.Errorf("upstream X-Drop = %q, want hop-by-hop removed", h.Get("X-Drop"))
	}
	if h.Get("Accept-Encoding") == "br" {
		t.Errorf("upstream Accept-Encoding = br, want caller value removed")
	}

	if resp.StatusCode != http.StatusCreated || resp.Header.Get("X-Upstream") != "yes" || string(body) != "ok" {
		t.Errorf("caller got %d %v %q", resp.StatusCode, resp.Header, body)
	}
	waitForLog(t, env.logs, "GET /open-apis/im/v1/chats 201")
	if strings.Contains(env.logs.String(), "t-first") {
		t.Errorf("logs leak token: %s", env.logs.String())
	}
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
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, resp)

	calls := rec.all()
	if len(calls) != 2 {
		t.Fatalf("api calls = %d, want 2", len(calls))
	}
	c := calls[0]
	if c.method != http.MethodPost || c.path != "/open-apis/im/v1/messages" || c.rawQuery != rawQuery {
		t.Errorf("upstream got %s %s ? %s", c.method, c.path, c.rawQuery)
	}
	if string(c.body) != jsonBody || c.header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Errorf("upstream body %q, Content-Type %q", c.body, c.header.Get("Content-Type"))
	}
	c = calls[1]
	if c.method != http.MethodPut || !bytes.Equal(c.body, multipartBody) {
		t.Errorf("multipart: method %s, body equal %v", c.method, bytes.Equal(c.body, multipartBody))
	}
	if c.header.Get("Content-Type") != mw.FormDataContentType() {
		t.Errorf("multipart Content-Type = %q", c.header.Get("Content-Type"))
	}
}

func TestForward_RejectsUnknownPath(t *testing.T) {
	env := newTestEnv(t)
	rec := &apiRecorder{}
	env.f.setAPI(func(w http.ResponseWriter, r *http.Request) { rec.record(r) })

	resp, err := http.Get(env.proxy.URL + "/foo")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if code, _ := proxyErr(t, body); code != -1 {
		t.Errorf("code = %d, want -1", code)
	}
	if n := len(rec.all()); n != 0 {
		t.Errorf("api calls = %d, want 0", n)
	}
}

func TestForward_BodyTooLarge(t *testing.T) {
	env := newTestEnv(t)
	env.s.maxBody = 10
	rec := &apiRecorder{}
	env.f.setAPI(func(w http.ResponseWriter, r *http.Request) { rec.record(r) })

	resp, err := http.Post(env.proxy.URL+"/open-apis/im/v1/messages", "text/plain", strings.NewReader("01234567890"))
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
	if code, _ := proxyErr(t, body); code != -1 {
		t.Errorf("code = %d, want -1", code)
	}
	if n := len(rec.all()); n != 0 {
		t.Errorf("api calls = %d, want 0", n)
	}
}

func TestForward_RetriesOnceOnInvalidToken(t *testing.T) {
	env := newTestEnv(t)
	env.f.setTokens("t-first", "t-second")
	rec := &apiRecorder{}
	env.f.setAPI(invalidTokenAPI(rec, false))
	if _, _, err := env.m.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	env.clock.Advance(61 * time.Second)

	resp, err := http.Post(env.proxy.URL+"/open-apis/im/v1/messages", "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK || string(body) != "hello" {
		t.Errorf("caller got %d %q, want 200 hello", resp.StatusCode, body)
	}
	calls := rec.all()
	if len(calls) != 2 {
		t.Fatalf("api calls = %d, want 2", len(calls))
	}
	if got := calls[1].header.Get("Authorization"); got != "Bearer t-second" {
		t.Errorf("retry Authorization = %q", got)
	}
	if string(calls[1].body) != "hello" {
		t.Errorf("retry body = %q", calls[1].body)
	}
	waitForLog(t, env.logs, "retried=true")
}

func TestForward_NoRetryWithinCooldown(t *testing.T) {
	env := newTestEnv(t)
	env.f.setTokens("t-first", "t-second")
	rec := &apiRecorder{}
	env.f.setAPI(invalidTokenAPI(rec, false))
	if _, _, err := env.m.Token(context.Background()); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(env.proxy.URL+"/open-apis/im/v1/messages", "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "99991663") {
		t.Errorf("caller got %d %q, want 400 with 99991663", resp.StatusCode, body)
	}
	if n := len(rec.all()); n != 1 {
		t.Errorf("api calls = %d, want 1", n)
	}
	if n := env.f.tokenHits.Load(); n != 1 {
		t.Errorf("tokenHits = %d, want 1", n)
	}
}

func TestForward_RetryWorksWhenCallerAcceptsGzip(t *testing.T) {
	env := newTestEnv(t)
	env.f.setTokens("t-first", "t-second")
	rec := &apiRecorder{}
	env.f.setAPI(invalidTokenAPI(rec, true))
	if _, _, err := env.m.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	env.clock.Advance(61 * time.Second)

	req, _ := http.NewRequest(http.MethodPost, env.proxy.URL+"/open-apis/im/v1/messages", strings.NewReader("hello"))
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if n := len(rec.all()); n != 2 {
		t.Errorf("api calls = %d, want 2", n)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "hello" {
		t.Errorf("caller got %d %q, want 200 hello", resp.StatusCode, body)
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want none", ce)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusBadRequest || string(body) != errBody {
		t.Errorf("caller got %d %q", resp.StatusCode, body)
	}
	if n := env.f.tokenHits.Load(); n != 1 {
		t.Errorf("tokenHits = %d, want 1", n)
	}
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
		if _, _, err := env.m.Token(context.Background()); err != nil {
			t.Fatal(err)
		}
		env.clock.Advance(61 * time.Second)

		resp, err := http.Get(env.proxy.URL + "/open-apis/im/v1/chats")
		if err != nil {
			t.Fatal(err)
		}
		body := readAll(t, resp)
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", resp.StatusCode)
		}
		if sha256.Sum256(body) != sha256.Sum256(large) {
			t.Errorf("body differs: got %d bytes, want %d", len(body), len(large))
		}
		if n := len(rec.all()); n != 1 {
			t.Errorf("api calls = %d, want 1", n)
		}
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
		if err != nil {
			t.Fatal(err)
		}
		body := readAll(t, resp)
		if resp.StatusCode != http.StatusBadGateway || string(body) != page || resp.Header.Get("Content-Type") != "text/html" {
			t.Errorf("caller got %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
		}
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
	if err != nil {
		t.Fatal(err)
	}
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
		t.Fatal("响应未流式透传")
	}
	closeRelease()
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := append(first, rest...)
	want := append(append([]byte(nil), head...), tail...)
	if len(got) != len(want) || sha256.Sum256(got) != sha256.Sum256(want) {
		t.Errorf("body length %d, want %d; hashes equal %v", len(got), len(want), sha256.Sum256(got) == sha256.Sum256(want))
	}
}

func TestForward_TokenUnavailable(t *testing.T) {
	env := newTestEnv(t)
	env.f.setFailCode(10003)

	resp, err := http.Get(env.proxy.URL + "/open-apis/im/v1/chats")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	code, msg := proxyErr(t, body)
	if code != -1 || !strings.Contains(msg, "tenant_access_token") {
		t.Errorf("error = %d %q", code, msg)
	}
	waitForLog(t, env.logs, "GET /open-apis/im/v1/chats 502")
	if strings.Contains(string(body), testAppSecret) || strings.Contains(env.logs.String(), testAppSecret) {
		t.Errorf("secret leaked: body %q logs %q", body, env.logs.String())
	}
}

func TestForward_UpstreamUnreachable(t *testing.T) {
	env := newTestEnv(t)
	if _, _, err := env.m.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	env.f.srv.Close()

	resp, err := http.Get(env.proxy.URL + "/open-apis/im/v1/chats")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if code, _ := proxyErr(t, body); code != -1 {
		t.Errorf("code = %d, want -1", code)
	}
}
