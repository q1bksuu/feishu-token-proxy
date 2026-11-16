package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

const maxRequestBody = 64 << 20

type Server struct {
	tokens  *TokenManager
	proxy   *httputil.ReverseProxy
	logger  *log.Logger
	maxBody int64
}

// NewServer 组装转发到 upstream 的反向代理。transport 是发往飞书的底层 RoundTripper，
// 外面包一层 tokenTransport 负责注入 token 和失效重放。
func NewServer(upstream *url.URL, tokens *TokenManager, transport http.RoundTripper, logger *log.Logger) *Server {
	s := &Server{tokens: tokens, logger: logger, maxBody: maxRequestBody}
	s.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			// ReverseProxy 在调用 Rewrite 前会重新编码无法解析的 query（含 ; 或孤立的 %），并丢掉相应参数；
			// 本服务不解析 query，这里换回调用方的原始 query 原样转发
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Header.Del("Authorization")
			// 由上游 Transport 自行协商 gzip 并自动解压，失效识别读到的是明文，调用方收到的也是明文
			pr.Out.Header.Del("Accept-Encoding")
		},
		Transport:     &tokenTransport{base: transport, tokens: tokens, logger: logger},
		FlushInterval: -1,
		ErrorLog:      logger,
		ErrorHandler:  s.proxyError,
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/", s.forward)
	mux.HandleFunc("GET /token", s.handleToken)
	mux.HandleFunc("GET /health", s.handleHealth)
	// "/" 兜底会匹配任意方法，/token、/health 的其他方法在这里显式返回 405
	methodNotAllowed := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "GET, HEAD")
		writeProxyError(w, http.StatusMethodNotAllowed, "仅支持 GET")
	}
	mux.HandleFunc("/token", methodNotAllowed)
	mux.HandleFunc("/health", methodNotAllowed)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeProxyError(w, http.StatusNotFound, "未知路径：仅支持 /open-apis/*、/token、/health")
	})
	return s.logRequests(localOnly(mux))
}

// localOnly 只放行本机程序直接发起的请求：Host 必须是 localhost 或回环 IP，且不带浏览器才会附加的 Origin 头
// 或非 none 的 Sec-Fetch-Site 头。这样本机浏览器里的网页无法借 DNS rebinding 或跨站请求读取 token、以应用身份调用飞书。
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		site := r.Header.Get("Sec-Fetch-Site")
		if !isLoopbackHost(host) || r.Header.Get("Origin") != "" || (site != "" && site != "none") {
			writeProxyError(w, http.StatusForbidden, "只接受本机程序直接发起的请求")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// forward 把请求体整体读入内存以便失效时重放，然后交给反向代理。
func (s *Server) forward(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeProxyError(w, http.StatusRequestEntityTooLarge, "请求体超过上限")
			return
		}
		writeProxyError(w, http.StatusBadRequest, "读取请求体失败")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.TransferEncoding = nil
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	s.proxy.ServeHTTP(w, r)
}

func (s *Server) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	var te *tokenError
	if errors.As(err, &te) {
		writeProxyError(w, http.StatusBadGateway, "获取 tenant_access_token 失败："+te.err.Error())
		return
	}
	s.logger.Printf("请求飞书失败：%s %s：%v", r.Method, r.URL.Path, err)
	writeProxyError(w, http.StatusBadGateway, "请求飞书失败："+err.Error())
}

type reqInfoKey struct{}

// reqInfo 随请求 ctx 传给 tokenTransport，用来把是否重放带回请求日志。
type reqInfo struct {
	retried bool
}

func requestInfo(ctx context.Context) *reqInfo {
	info, _ := ctx.Value(reqInfoKey{}).(*reqInfo)
	return info
}

// statusRecorder 记下最终状态码；Unwrap 让 http.ResponseController 的 Flush 穿透到底层连接。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (rec *statusRecorder) WriteHeader(code int) {
	if rec.status == 0 && code >= 200 {
		rec.status = code
	}
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *statusRecorder) Write(b []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	return rec.ResponseWriter.Write(b)
}

func (rec *statusRecorder) Unwrap() http.ResponseWriter {
	return rec.ResponseWriter
}

// logRequests 每个请求记一行：方法、路径（不含 query）、状态码、耗时、是否重放。
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &reqInfo{}
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), reqInfoKey{}, info)))
		s.logger.Printf("%s %s %d %dms retried=%t", r.Method, r.URL.Path, rec.status, time.Since(start).Milliseconds(), info.retried)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeProxyError 输出代理自身的错误：{"code":-1,"msg":"feishu-token-proxy: <msg>"}。
func writeProxyError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}{-1, "feishu-token-proxy: " + msg})
}
