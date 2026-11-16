package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// maxErrorPeek 是检查失效错误码时最多读入内存的响应字节数，超过的响应直接透传。
const maxErrorPeek = 1 << 20

// retryableCodes 是表示 token 无效的飞书错误码，命中时强制刷新 token 并重放一次。
var retryableCodes = map[int]bool{99991663: true, 99991665: true}

// tokenTransport 给发往飞书的请求注入 tenant_access_token；飞书判定 token 无效时强制刷新并重放一次。
// 请求须带 GetBody（Server.forward 负责设置），重放时据此拿到完整的请求体。
type tokenTransport struct {
	base   http.RoundTripper
	tokens *TokenManager
	logger *log.Logger
}

// tokenError 表示拿不到可用 token，ErrorHandler 据此给出专门的错误信息。
type tokenError struct{ err error }

func (e *tokenError) Error() string { return "获取 tenant_access_token 失败：" + e.err.Error() }
func (e *tokenError) Unwrap() error { return e.err }

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	tok, _, err := t.tokens.Token(ctx)
	if err != nil {
		return nil, &tokenError{err}
	}
	resp, err := t.send(req, tok)
	if err != nil {
		return nil, err
	}

	code, err := peekErrorCode(resp)
	if err != nil {
		return nil, err
	}
	if !retryableCodes[code] {
		return resp, nil
	}

	newTok, ok, err := t.tokens.ForceRefresh(ctx, tok)
	if err != nil {
		t.logger.Printf("token 失效（code=%d），刷新失败：%v", code, err)
	}
	if !ok {
		return resp, nil
	}
	resp.Body.Close()
	if info := requestInfo(ctx); info != nil {
		info.retried = true
	}
	t.logger.Printf("token 失效（code=%d），已刷新并重放", code)
	return t.send(req, newTok)
}

// send 克隆请求、换上新的请求体并注入 token 后发出。无请求体的请求保持 Body 为 nil。
func (t *tokenTransport) send(req *http.Request, tok string) (*http.Response, error) {
	out := req.Clone(req.Context())
	if req.Body != nil && req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		out.Body = body
	}
	out.Header.Set("Authorization", "Bearer "+tok)
	return t.base.RoundTrip(out)
}

// peekErrorCode 对非 2xx 的 JSON 响应读出至多 maxErrorPeek+1 字节解析 code，并把已读部分接回 resp.Body。
// 响应体超过 maxErrorPeek 时不解析，返回 0。
func peekErrorCode(resp *http.Response) (int, error) {
	if resp.StatusCode < 300 || !strings.Contains(resp.Header.Get("Content-Type"), "json") {
		return 0, nil
	}
	peek, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorPeek+1))
	if err != nil {
		resp.Body.Close()
		return 0, err
	}
	resp.Body = &peekedBody{Reader: io.MultiReader(bytes.NewReader(peek), resp.Body), Closer: resp.Body}
	if len(peek) > maxErrorPeek {
		return 0, nil
	}
	var e struct {
		Code int `json:"code"`
	}
	if json.Unmarshal(peek, &e) != nil {
		return 0, nil
	}
	return e.Code, nil
}

type peekedBody struct {
	io.Reader
	io.Closer
}

// NewUpstreamTransport 返回发往飞书的 Transport：沿用 http.DefaultTransport 的设置（含 HTTPS_PROXY 等环境变量），
// 等待响应头最多 60 秒；响应体读取不设总超时，大文件下载可以持续进行。
func NewUpstreamTransport() http.RoundTripper {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 60 * time.Second
	return tr
}
