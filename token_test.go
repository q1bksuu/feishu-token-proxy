package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestToken_FetchesOnceAndCaches(t *testing.T) {
	f := newFakeFeishu(t)
	m, clock, logs := newTestManager(t, f)
	ctx := context.Background()

	for i := range 2 {
		tok, exp, err := m.Token(ctx)
		if err != nil {
			t.Fatalf("Token #%d: %v", i, err)
		}
		if tok != "t-first" {
			t.Errorf("Token #%d = %q", i, tok)
		}
		if want := clock.Now().Add(7200 * time.Second); !exp.Equal(want) {
			t.Errorf("expiresAt = %v, want %v", exp, want)
		}
	}
	if n := f.tokenHits.Load(); n != 1 {
		t.Errorf("tokenHits = %d, want 1", n)
	}
	body, ct := f.snapshot()
	if body["app_id"] != testAppID || body["app_secret"] != testAppSecret || len(body) != 2 {
		t.Errorf("token request body = %v", body)
	}
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	if l := logs.String(); strings.Contains(l, "t-first") || strings.Contains(l, testAppSecret) {
		t.Errorf("logs leak token or secret: %s", l)
	}
}

func TestToken_RefetchesNearExpiry(t *testing.T) {
	f := newFakeFeishu(t)
	f.setTokens("t-first", "t-second")
	m, clock, _ := newTestManager(t, f)
	ctx := context.Background()

	if tok, _, _ := m.Token(ctx); tok != "t-first" {
		t.Fatalf("Token = %q", tok)
	}
	clock.Advance(7200*time.Second - 59*time.Second)
	tok, _, err := m.Token(ctx)
	if err != nil || tok != "t-second" {
		t.Errorf("Token = %q, %v; want t-second", tok, err)
	}
	if n := f.tokenHits.Load(); n != 2 {
		t.Errorf("tokenHits = %d, want 2", n)
	}
}

func TestToken_ConcurrentCallersShareOneFetch(t *testing.T) {
	f := newFakeFeishu(t)
	f.setTokenDelay(100 * time.Millisecond)
	m, _, _ := newTestManager(t, f)

	var wg sync.WaitGroup
	results := make([]string, 50)
	errs := make([]error, 50)
	for i := range results {
		wg.Go(func() {
			results[i], _, errs[i] = m.Token(context.Background())
		})
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i] != "t-first" {
			t.Errorf("caller %d: %q, %v", i, results[i], errs[i])
		}
	}
	if n := f.tokenHits.Load(); n != 1 {
		t.Errorf("tokenHits = %d, want 1", n)
	}
}

func TestToken_FailureBackoff(t *testing.T) {
	f := newFakeFeishu(t)
	f.setFailCode(10003)
	m, clock, logs := newTestManager(t, f)
	ctx := context.Background()

	_, _, err := m.Token(ctx)
	if err == nil || !strings.Contains(err.Error(), "10003") {
		t.Fatalf("err = %v, want code 10003", err)
	}
	if strings.Contains(err.Error(), testAppSecret) {
		t.Errorf("error leaks secret: %v", err)
	}

	_, _, err = m.Token(ctx)
	if err == nil {
		t.Error("second Token within backoff: want error")
	}
	if n := f.tokenHits.Load(); n != 1 {
		t.Errorf("tokenHits within backoff = %d, want 1", n)
	}

	clock.Advance(6 * time.Second)
	_, _, _ = m.Token(ctx)
	if n := f.tokenHits.Load(); n != 2 {
		t.Errorf("tokenHits after backoff = %d, want 2", n)
	}
	if strings.Contains(logs.String(), testAppSecret) {
		t.Errorf("logs leak secret: %s", logs.String())
	}
}

func TestRefreshIfNeeded(t *testing.T) {
	f := newFakeFeishu(t)
	f.setTokens("t-first", "t-second")
	m, clock, _ := newTestManager(t, f)
	ctx := context.Background()

	if _, _, err := m.Token(ctx); err != nil {
		t.Fatal(err)
	}
	clock.Advance(7200*time.Second - 30*time.Minute)
	m.RefreshIfNeeded(ctx)
	if n := f.tokenHits.Load(); n != 1 {
		t.Errorf("tokenHits with 30min left = %d, want 1", n)
	}

	clock.Advance(6 * time.Minute)
	m.RefreshIfNeeded(ctx)
	if n := f.tokenHits.Load(); n != 2 {
		t.Errorf("tokenHits with 24min left = %d, want 2", n)
	}
	if tok, _, _ := m.Token(ctx); tok != "t-second" {
		t.Errorf("Token = %q, want t-second", tok)
	}
}

func TestRefreshIfNeeded_FailureKeepsOldToken(t *testing.T) {
	f := newFakeFeishu(t)
	m, clock, _ := newTestManager(t, f)
	ctx := context.Background()

	if tok, _, _ := m.Token(ctx); tok != "t-first" {
		t.Fatalf("Token = %q", tok)
	}
	f.setFailCode(10003)
	clock.Advance(7200*time.Second - 20*time.Minute)
	m.RefreshIfNeeded(ctx)
	hits := f.tokenHits.Load()

	tok, _, err := m.Token(ctx)
	if err != nil || tok != "t-first" {
		t.Errorf("Token = %q, %v; want t-first", tok, err)
	}
	if n := f.tokenHits.Load(); n != hits {
		t.Errorf("Token issued a new fetch: hits %d → %d", hits, n)
	}
	if s := m.Status(); !strings.Contains(s.LastError, "10003") {
		t.Errorf("Status().LastError = %q", s.LastError)
	}
}

func TestForceRefresh(t *testing.T) {
	ctx := context.Background()

	t.Run("冷却期内不刷新", func(t *testing.T) {
		f := newFakeFeishu(t)
		m, _, _ := newTestManager(t, f)
		if _, _, err := m.Token(ctx); err != nil {
			t.Fatal(err)
		}
		tok, ok, err := m.ForceRefresh(ctx, "t-first")
		if tok != "" || ok || err != nil {
			t.Errorf("ForceRefresh = %q, %v, %v; want \"\", false, nil", tok, ok, err)
		}
		if n := f.tokenHits.Load(); n != 1 {
			t.Errorf("tokenHits = %d, want 1", n)
		}
	})

	t.Run("冷却期后换新，已被换掉则直接复用", func(t *testing.T) {
		f := newFakeFeishu(t)
		f.setTokens("t-first", "t-second")
		m, clock, _ := newTestManager(t, f)
		if _, _, err := m.Token(ctx); err != nil {
			t.Fatal(err)
		}
		clock.Advance(61 * time.Second)
		tok, ok, err := m.ForceRefresh(ctx, "t-first")
		if tok != "t-second" || !ok || err != nil {
			t.Errorf("ForceRefresh = %q, %v, %v; want t-second, true, nil", tok, ok, err)
		}
		if n := f.tokenHits.Load(); n != 2 {
			t.Errorf("tokenHits = %d, want 2", n)
		}

		tok, ok, err = m.ForceRefresh(ctx, "t-first")
		if tok != "t-second" || !ok || err != nil {
			t.Errorf("second ForceRefresh = %q, %v, %v; want t-second, true, nil", tok, ok, err)
		}
		if n := f.tokenHits.Load(); n != 2 {
			t.Errorf("tokenHits after reuse = %d, want 2", n)
		}
	})

	t.Run("飞书返回同一 token", func(t *testing.T) {
		f := newFakeFeishu(t)
		m, clock, _ := newTestManager(t, f)
		if _, _, err := m.Token(ctx); err != nil {
			t.Fatal(err)
		}
		clock.Advance(61 * time.Second)
		tok, ok, err := m.ForceRefresh(ctx, "t-first")
		if tok != "" || ok || err != nil {
			t.Errorf("ForceRefresh = %q, %v, %v; want \"\", false, nil", tok, ok, err)
		}
		if n := f.tokenHits.Load(); n != 2 {
			t.Errorf("tokenHits = %d, want 2", n)
		}
	})
}

func TestForceRefresh_ConcurrentCallersFetchOnce(t *testing.T) {
	f := newFakeFeishu(t)
	f.setTokens("t-first", "t-second")
	m, clock, _ := newTestManager(t, f)
	ctx := context.Background()
	if _, _, err := m.Token(ctx); err != nil {
		t.Fatal(err)
	}
	clock.Advance(61 * time.Second)

	type result struct {
		tok string
		ok  bool
		err error
	}
	results := make([]result, 20)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			r := &results[i]
			r.tok, r.ok, r.err = m.ForceRefresh(ctx, "t-first")
		})
	}
	wg.Wait()
	for i, r := range results {
		if r.tok != "t-second" || !r.ok || r.err != nil {
			t.Errorf("caller %d: %q, %v, %v", i, r.tok, r.ok, r.err)
		}
	}
	if n := f.tokenHits.Load(); n != 2 {
		t.Errorf("tokenHits = %d, want 2", n)
	}
}

func TestStatus(t *testing.T) {
	f := newFakeFeishu(t)
	m, clock, _ := newTestManager(t, f)

	s := m.Status()
	if s.Valid || s.ExpiresIn != 0 {
		t.Errorf("initial Status = %+v", s)
	}

	if _, _, err := m.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	s = m.Status()
	if !s.Valid || s.ExpiresIn != 7200*time.Second || !s.LastRefresh.Equal(clock.Now()) || s.LastError != "" {
		t.Errorf("Status after Token = %+v", s)
	}
}
