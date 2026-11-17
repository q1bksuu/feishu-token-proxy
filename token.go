package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	tokenPath            = "/open-apis/auth/v3/tenant_access_token/internal"
	refreshBefore        = 25 * time.Minute // 剩余有效期不足此值时后台换新
	refreshCheckInterval = time.Minute
	expirySkew           = 5 * time.Minute // 距过期不足此值即视为不可用
	forceRefreshCooldown = 60 * time.Second
	failureBackoff       = 5 * time.Second // 一次获取结束后此时间内，Token 直接复用它的结果（失败则返回同一个错误）
	fetchTimeout         = 10 * time.Second
)

// TokenManager 获取并缓存 tenant_access_token。
// 所有对飞书鉴权接口的请求都在 fetchMu 下串行进行，并发调用方等待并复用同一次获取的结果；
// mu 只保护状态字段，读状态时不会被进行中的获取阻塞。
type TokenManager struct {
	tokenURL  string
	appID     string
	appSecret string
	client    *http.Client
	logger    *log.Logger
	now       func() time.Time

	fetchMu sync.Mutex

	mu          sync.Mutex
	token       string
	expiresAt   time.Time
	lastDone    time.Time // 最近一次获取结束的时刻，无论成败
	lastRefresh time.Time
	lastErr     error
}

type TokenStatus struct {
	Valid       bool
	ExpiresIn   time.Duration // 无 token 或已过期时为 0
	LastRefresh time.Time     // 最近一次成功获取的时刻
	LastError   string        // 最近一次失败原因，成功后清空
}

func NewTokenManager(upstream *url.URL, appID, appSecret string, client *http.Client, logger *log.Logger) *TokenManager {
	u := *upstream
	u.Path = tokenPath
	return &TokenManager{
		tokenURL:  u.String(),
		appID:     appID,
		appSecret: appSecret,
		client:    client,
		logger:    logger,
		now:       time.Now,
	}
}

// Token 返回可用的 token 及其过期时刻；不可用时同步获取。
// 最近一次获取结束不足 failureBackoff 时复用它的结果：失败则返回同一个错误，成功则返回那次拿到的 token。
// 因此排队等锁的调用方共享刚结束的那次获取，飞书给的有效期过短时也不会每个请求都去获取。
// 需要发起新获取而 ctx 已结束时直接返回 ctx 的错误。
func (m *TokenManager) Token(ctx context.Context) (string, time.Time, error) {
	if tok, exp, ok := m.usable(); ok {
		return tok, exp, nil
	}

	m.fetchMu.Lock()
	defer m.fetchMu.Unlock()

	if tok, exp, ok := m.usable(); ok {
		return tok, exp, nil
	}
	if tok, exp, ok, err := m.recentResult(); ok {
		return tok, exp, err
	}
	if err := ctx.Err(); err != nil {
		return "", time.Time{}, err
	}
	if err := m.fetch(ctx); err != nil {
		return "", time.Time{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.token, m.expiresAt, nil
}

// ForceRefresh 在调用方发现 bad 已被飞书判定失效时调用。
// 当前 token 已不是 bad 且可用时直接返回它；距上次获取不足 forceRefreshCooldown 时返回 ok=false；
// 否则向飞书重新获取，拿到与 bad 不同的 token 时返回 ok=true。
func (m *TokenManager) ForceRefresh(ctx context.Context, bad string) (string, bool, error) {
	m.fetchMu.Lock()
	defer m.fetchMu.Unlock()

	if tok, _, ok := m.usable(); ok && tok != bad {
		return tok, true, nil
	}
	m.mu.Lock()
	lastDone := m.lastDone
	m.mu.Unlock()
	if m.now().Sub(lastDone) < forceRefreshCooldown {
		return "", false, nil
	}
	if err := m.fetch(ctx); err != nil {
		return "", false, err
	}
	m.mu.Lock()
	tok := m.token
	m.mu.Unlock()
	if tok == bad {
		return "", false, nil
	}
	return tok, true, nil
}

// RefreshIfNeeded 在 token 不可用或剩余有效期不足 refreshBefore 时重新获取；失败只记日志，旧 token 继续可用直到过期。
func (m *TokenManager) RefreshIfNeeded(ctx context.Context) {
	m.fetchMu.Lock()
	defer m.fetchMu.Unlock()

	if _, exp, ok := m.usable(); ok && exp.Sub(m.now()) >= refreshBefore {
		return
	}
	_ = m.fetch(ctx)
}

// Run 每 refreshCheckInterval 调一次 RefreshIfNeeded，直到 ctx 结束。
func (m *TokenManager) Run(ctx context.Context) {
	ticker := time.NewTicker(refreshCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.RefreshIfNeeded(ctx)
		}
	}
}

func (m *TokenManager) Status() TokenStatus {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	s := TokenStatus{
		Valid:       m.token != "" && now.Before(m.expiresAt.Add(-expirySkew)),
		LastRefresh: m.lastRefresh,
	}
	if m.token != "" && now.Before(m.expiresAt) {
		s.ExpiresIn = m.expiresAt.Sub(now)
	}
	if m.lastErr != nil {
		s.LastError = m.lastErr.Error()
	}
	return s
}

// usable 返回当前 token，ok 表示它存在且距过期超过 expirySkew。
func (m *TokenManager) usable() (string, time.Time, bool) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	ok := m.token != "" && now.Before(m.expiresAt.Add(-expirySkew))
	return m.token, m.expiresAt, ok
}

// recentResult 在最近一次获取结束不足 failureBackoff 时返回它的结果，ok 表示结果可复用。
// 那次获取成功但 token 已过期时不可复用。
func (m *TokenManager) recentResult() (string, time.Time, bool, error) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastDone.IsZero() || now.Sub(m.lastDone) >= failureBackoff {
		return "", time.Time{}, false, nil
	}
	if m.lastErr != nil {
		return "", time.Time{}, true, m.lastErr
	}
	if m.token != "" && now.Before(m.expiresAt) {
		return m.token, m.expiresAt, true, nil
	}
	return "", time.Time{}, false, nil
}

type tokenResponse struct {
	Code              int    `json:"code"`
	Msg               string `json:"msg"`
	TenantAccessToken string `json:"tenant_access_token"`
	Expire            int    `json:"expire"`
}

// fetch 向飞书获取 token 并更新状态，调用方须持有 fetchMu。
// 请求脱离调用方 ctx 的取消，避免一个调用方断开连接让共享这次获取的其他调用方一起失败。
func (m *TokenManager) fetch(ctx context.Context) error {
	start := m.now()
	tok, expire, err := m.request(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastDone = m.now()
	if err != nil {
		m.lastErr = err
		m.logger.Printf("获取 tenant_access_token 失败：%v", err)
		return err
	}
	m.token = tok
	m.expiresAt = start.Add(time.Duration(expire) * time.Second)
	m.lastRefresh = start
	m.lastErr = nil
	m.logger.Printf("已获取 tenant_access_token，有效期 %ds", expire)
	return nil
}

func (m *TokenManager) request(ctx context.Context) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
	defer cancel()

	payload, err := json.Marshal(map[string]string{"app_id": m.appID, "app_secret": m.appSecret})
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.tokenURL, bytes.NewReader(payload))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := m.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("请求飞书鉴权接口失败：%w", err)
	}
	defer resp.Body.Close()

	var r tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return "", 0, fmt.Errorf("飞书鉴权接口返回 HTTP %d，响应无法解析", resp.StatusCode)
	}
	if r.Code != 0 {
		return "", 0, fmt.Errorf("飞书返回 code=%d msg=%s", r.Code, r.Msg)
	}
	if r.TenantAccessToken == "" {
		return "", 0, errors.New("飞书返回的 tenant_access_token 为空")
	}
	if r.Expire <= 0 {
		return "", 0, fmt.Errorf("飞书返回的 expire 无效：%d", r.Expire)
	}
	return r.TenantAccessToken, r.Expire, nil
}
