package main

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToken_FetchesOnceAndCaches(t *testing.T) {
	f := newFakeFeishu(t)
	m, clock, logs := newTestManager(t, f)
	ctx := context.Background()

	for i := range 2 {
		tok, exp, err := m.Token(ctx)
		require.NoError(t, err, "Token #%d", i)
		assert.Equal(t, "t-first", tok, "Token #%d", i)
		assert.WithinDuration(t, clock.Now().Add(7200*time.Second), exp, 0, "Token #%d expiresAt", i)
	}
	assert.EqualValues(t, 1, f.tokenHits.Load(), "tokenHits")
	body, ct := f.snapshot()
	assert.Equal(t, testAppID, body["app_id"], "token request app_id")
	assert.Equal(t, testAppSecret, body["app_secret"], "token request app_secret")
	assert.Len(t, body, 2, "token request body")
	assert.True(t, strings.HasPrefix(ct, "application/json"), "Content-Type = %q", ct)
	assert.NotContains(t, logs.String(), "t-first", "logs leak token")
	assert.NotContains(t, logs.String(), testAppSecret, "logs leak secret")
}

func TestToken_RefetchesNearExpiry(t *testing.T) {
	f := newFakeFeishu(t)
	f.setTokens("t-first", "t-second")
	m, clock, _ := newTestManager(t, f)
	ctx := context.Background()

	first, _, _ := m.Token(ctx)
	require.Equal(t, "t-first", first)

	clock.Advance(7200*time.Second - 5*time.Minute - time.Second)
	tok, _, err := m.Token(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "t-first", tok, "5min+1s left")
	assert.EqualValues(t, 1, f.tokenHits.Load(), "tokenHits with 5min+1s left")

	clock.Advance(2 * time.Second)
	tok, _, err = m.Token(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "t-second", tok, "4min59s left")
	assert.EqualValues(t, 2, f.tokenHits.Load(), "tokenHits with 4min59s left")
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
		assert.NoError(t, errs[i], "caller %d", i)
		assert.Equal(t, "t-first", results[i], "caller %d", i)
	}
	assert.EqualValues(t, 1, f.tokenHits.Load(), "tokenHits")
}

func TestToken_FailureBackoff(t *testing.T) {
	f := newFakeFeishu(t)
	f.setFailCode(10003)
	m, clock, logs := newTestManager(t, f)
	ctx := context.Background()

	_, _, err := m.Token(ctx)
	require.ErrorContains(t, err, "10003")
	assert.NotContains(t, err.Error(), testAppSecret, "error leaks secret")

	_, _, err = m.Token(ctx)
	assert.Error(t, err, "second Token within backoff")
	assert.EqualValues(t, 1, f.tokenHits.Load(), "tokenHits within backoff")

	clock.Advance(6 * time.Second)
	_, _, _ = m.Token(ctx)
	assert.EqualValues(t, 2, f.tokenHits.Load(), "tokenHits after backoff")
	assert.NotContains(t, logs.String(), testAppSecret, "logs leak secret")
}

func TestRefreshIfNeeded(t *testing.T) {
	f := newFakeFeishu(t)
	f.setTokens("t-first", "t-second")
	m, clock, _ := newTestManager(t, f)
	ctx := context.Background()

	_, _, err := m.Token(ctx)
	require.NoError(t, err)
	clock.Advance(7200*time.Second - 30*time.Minute)
	m.RefreshIfNeeded(ctx)
	assert.EqualValues(t, 1, f.tokenHits.Load(), "tokenHits with 30min left")

	clock.Advance(6 * time.Minute)
	m.RefreshIfNeeded(ctx)
	assert.EqualValues(t, 2, f.tokenHits.Load(), "tokenHits with 24min left")
	tok, _, _ := m.Token(ctx)
	assert.Equal(t, "t-second", tok)
}

func TestRefreshIfNeeded_FailureKeepsOldToken(t *testing.T) {
	f := newFakeFeishu(t)
	m, clock, _ := newTestManager(t, f)
	ctx := context.Background()

	first, _, _ := m.Token(ctx)
	require.Equal(t, "t-first", first)
	f.setFailCode(10003)
	clock.Advance(7200*time.Second - 20*time.Minute)
	m.RefreshIfNeeded(ctx)
	hits := f.tokenHits.Load()

	tok, _, err := m.Token(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "t-first", tok)
	assert.Equal(t, hits, f.tokenHits.Load(), "Token issued a new fetch")
	assert.Contains(t, m.Status().LastError, "10003", "Status().LastError")
}

func TestForceRefresh(t *testing.T) {
	ctx := context.Background()

	t.Run("冷却期内不刷新", func(t *testing.T) {
		f := newFakeFeishu(t)
		m, _, _ := newTestManager(t, f)
		_, _, err := m.Token(ctx)
		require.NoError(t, err)
		tok, ok, err := m.ForceRefresh(ctx, "t-first")
		assert.Empty(t, tok, "ForceRefresh token")
		assert.False(t, ok, "ForceRefresh ok")
		assert.NoError(t, err, "ForceRefresh")
		assert.EqualValues(t, 1, f.tokenHits.Load(), "tokenHits")
	})

	t.Run("冷却期后换新，已被换掉则直接复用", func(t *testing.T) {
		f := newFakeFeishu(t)
		f.setTokens("t-first", "t-second")
		m, clock, _ := newTestManager(t, f)
		_, _, err := m.Token(ctx)
		require.NoError(t, err)
		clock.Advance(61 * time.Second)
		tok, ok, err := m.ForceRefresh(ctx, "t-first")
		assert.Equal(t, "t-second", tok, "ForceRefresh token")
		assert.True(t, ok, "ForceRefresh ok")
		assert.NoError(t, err, "ForceRefresh")
		assert.EqualValues(t, 2, f.tokenHits.Load(), "tokenHits")

		tok, ok, err = m.ForceRefresh(ctx, "t-first")
		assert.Equal(t, "t-second", tok, "second ForceRefresh token")
		assert.True(t, ok, "second ForceRefresh ok")
		assert.NoError(t, err, "second ForceRefresh")
		assert.EqualValues(t, 2, f.tokenHits.Load(), "tokenHits after reuse")
	})

	t.Run("飞书返回同一 token", func(t *testing.T) {
		f := newFakeFeishu(t)
		m, clock, _ := newTestManager(t, f)
		_, _, err := m.Token(ctx)
		require.NoError(t, err)
		clock.Advance(61 * time.Second)
		tok, ok, err := m.ForceRefresh(ctx, "t-first")
		assert.Empty(t, tok, "ForceRefresh token")
		assert.False(t, ok, "ForceRefresh ok")
		assert.NoError(t, err, "ForceRefresh")
		assert.EqualValues(t, 2, f.tokenHits.Load(), "tokenHits")
	})
}

func TestForceRefresh_ConcurrentCallersFetchOnce(t *testing.T) {
	f := newFakeFeishu(t)
	f.setTokens("t-first", "t-second")
	m, clock, _ := newTestManager(t, f)
	ctx := context.Background()
	_, _, err := m.Token(ctx)
	require.NoError(t, err)
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
		assert.Equal(t, "t-second", r.tok, "caller %d token", i)
		assert.True(t, r.ok, "caller %d ok", i)
		assert.NoError(t, r.err, "caller %d", i)
	}
	assert.EqualValues(t, 2, f.tokenHits.Load(), "tokenHits")
}

func TestStatus(t *testing.T) {
	f := newFakeFeishu(t)
	m, clock, _ := newTestManager(t, f)

	s := m.Status()
	assert.False(t, s.Valid, "initial Valid")
	assert.Zero(t, s.ExpiresIn, "initial ExpiresIn")

	_, _, err := m.Token(context.Background())
	require.NoError(t, err)
	s = m.Status()
	assert.True(t, s.Valid, "Valid after Token")
	assert.Equal(t, 7200*time.Second, s.ExpiresIn, "ExpiresIn after Token")
	assert.WithinDuration(t, clock.Now(), s.LastRefresh, 0, "LastRefresh after Token")
	assert.Empty(t, s.LastError, "LastError after Token")

	clock.Advance(7200*time.Second - 5*time.Minute + time.Second)
	s = m.Status()
	assert.False(t, s.Valid, "Valid with 4min59s left")
	assert.Equal(t, 5*time.Minute-time.Second, s.ExpiresIn, "ExpiresIn with 4min59s left")
}

func TestToken_WaitersReuseSlowFailedFetch(t *testing.T) {
	f := newFakeFeishu(t)
	f.setFailCode(10003)
	f.setTokenDelay(100 * time.Millisecond)
	m, clock, _ := newTestManager(t, f)
	m.client = &http.Client{Transport: &advancingTransport{base: f.srv.Client().Transport, clock: clock, d: 6 * time.Second}}

	errs := make([]error, 10)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() {
			_, _, errs[i] = m.Token(context.Background())
		})
	}
	wg.Wait()
	for i, err := range errs {
		assert.ErrorContains(t, err, "10003", "caller %d", i)
	}
	assert.EqualValues(t, 1, f.tokenHits.Load(), "tokenHits")
}

func TestToken_CanceledCallerDoesNotFetch(t *testing.T) {
	f := newFakeFeishu(t)
	f.setFailCode(10003)
	m, clock, _ := newTestManager(t, f)
	_, _, err := m.Token(context.Background())
	require.Error(t, err, "want first Token to fail")
	clock.Advance(6 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = m.Token(ctx)
	assert.ErrorIs(t, err, context.Canceled)
	assert.EqualValues(t, 1, f.tokenHits.Load(), "tokenHits")
}

func TestToken_ShortExpireThrottled(t *testing.T) {
	f := newFakeFeishu(t)
	f.setExpire(30)
	m, clock, _ := newTestManager(t, f)

	for i := range 20 {
		tok, _, err := m.Token(context.Background())
		require.NoError(t, err, "Token #%d", i)
		require.Equal(t, "t-first", tok, "Token #%d", i)
	}
	assert.EqualValues(t, 1, f.tokenHits.Load(), "tokenHits")

	clock.Advance(6 * time.Second)
	_, _, _ = m.Token(context.Background())
	assert.EqualValues(t, 2, f.tokenHits.Load(), "tokenHits after backoff")
}

func TestToken_RejectsNonPositiveExpire(t *testing.T) {
	f := newFakeFeishu(t)
	f.setExpire(0)
	m, _, _ := newTestManager(t, f)

	_, _, err := m.Token(context.Background())
	require.ErrorContains(t, err, "expire")
	_, _, _ = m.Token(context.Background())
	assert.EqualValues(t, 1, f.tokenHits.Load(), "tokenHits (failure backoff)")
}

// lazyTestSetup 取到 t-first（第 1 次获取）后把时钟推进到剩余 left。
func lazyTestSetup(t *testing.T, m *TokenManager, clock *fakeClock, left time.Duration) {
	t.Helper()
	tok, _, err := m.Token(context.Background())
	require.NoError(t, err)
	require.Equal(t, "t-first", tok)
	clock.Advance(7200*time.Second - left)
}

// concurrentTokens 并发 n 个 Token 调用，全部应无错误并返回 want。
func concurrentTokens(t *testing.T, m *TokenManager, n int, want string) {
	t.Helper()
	results := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			results[i], _, errs[i] = m.Token(context.Background())
		})
	}
	wg.Wait()
	for i := range results {
		assert.NoError(t, errs[i], "caller %d", i)
		assert.Equal(t, want, results[i], "caller %d", i)
	}
}

func TestToken_EagerModeNoAsyncRefresh(t *testing.T) {
	f := newFakeFeishu(t)
	m, clock, _ := newTestManager(t, f)
	lazyTestSetup(t, m, clock, 20*time.Minute)

	tok, _, err := m.Token(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, "t-first", tok)
	assert.False(t, m.refreshing.Load(), "async refresh started in eager mode")
	assert.EqualValues(t, 1, f.tokenHits.Load(), "tokenHits")
}

func TestLazy_NoFetchWithEnoughTimeLeft(t *testing.T) {
	f := newFakeFeishu(t)
	m, clock, _ := newLazyTestManager(t, f)
	lazyTestSetup(t, m, clock, 25*time.Minute)

	tok, _, err := m.Token(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, "t-first", tok)
	assert.False(t, m.refreshing.Load(), "async refresh started with 25min left")
	waitRefreshIdle(t, m)
	assert.EqualValues(t, 1, f.tokenHits.Load(), "tokenHits")
}

func TestLazy_AsyncRefreshWithinWindow(t *testing.T) {
	f := newFakeFeishu(t)
	f.setTokens("t-first", "t-second")
	f.setTokenDelay(100 * time.Millisecond)
	m, clock, _ := newLazyTestManager(t, f)
	lazyTestSetup(t, m, clock, 20*time.Minute)

	concurrentTokens(t, m, 50, "t-first")
	waitRefreshIdle(t, m)
	assert.EqualValues(t, 2, f.tokenHits.Load(), "tokenHits")
	tok, _, err := m.Token(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, "t-second", tok, "Token after async refresh")
}

func TestLazy_AsyncRefreshSingleFlight(t *testing.T) {
	f := newFakeFeishu(t)
	m, clock, _ := newLazyTestManager(t, f)
	lazyTestSetup(t, m, clock, 20*time.Minute)
	f.setFailCode(10003)
	f.setTokenDelay(100 * time.Millisecond)

	concurrentTokens(t, m, 50, "t-first")
	waitRefreshIdle(t, m)
	assert.EqualValues(t, 2, f.tokenHits.Load(), "tokenHits")
	assert.Contains(t, m.Status().LastError, "10003", "Status().LastError")
}

func TestLazy_AsyncRefreshBackoff(t *testing.T) {
	f := newFakeFeishu(t)
	m, clock, _ := newLazyTestManager(t, f)
	lazyTestSetup(t, m, clock, 20*time.Minute)
	f.setFailCode(10003)
	ctx := context.Background()

	_, _, _ = m.Token(ctx)
	waitRefreshIdle(t, m)
	assert.EqualValues(t, 2, f.tokenHits.Load(), "tokenHits after first async refresh")

	clock.Advance(4 * time.Second)
	_, _, _ = m.Token(ctx)
	waitRefreshIdle(t, m)
	assert.EqualValues(t, 2, f.tokenHits.Load(), "tokenHits within backoff")

	clock.Advance(2 * time.Second)
	_, _, _ = m.Token(ctx)
	waitRefreshIdle(t, m)
	assert.EqualValues(t, 3, f.tokenHits.Load(), "tokenHits after backoff")
}

func TestLazy_ShortExpireThrottled(t *testing.T) {
	f := newFakeFeishu(t)
	f.setExpire(20 * 60)
	f.setTokens("t-first", "t-second")
	m, clock, _ := newLazyTestManager(t, f)
	ctx := context.Background()

	for i := range 21 {
		tok, _, err := m.Token(ctx)
		require.NoError(t, err, "Token #%d", i)
		require.Equal(t, "t-first", tok, "Token #%d", i)
	}
	waitRefreshIdle(t, m)
	assert.EqualValues(t, 1, f.tokenHits.Load(), "tokenHits within backoff")

	clock.Advance(6 * time.Second)
	_, _, _ = m.Token(ctx)
	waitRefreshIdle(t, m)
	assert.EqualValues(t, 2, f.tokenHits.Load(), "tokenHits after backoff")

	_, _, _ = m.Token(ctx)
	waitRefreshIdle(t, m)
	assert.EqualValues(t, 2, f.tokenHits.Load(), "tokenHits right after refresh")
}

func TestLazy_SyncFetchUnderFiveMinutes(t *testing.T) {
	f := newFakeFeishu(t)
	f.setTokens("t-first", "t-second")
	m, clock, _ := newLazyTestManager(t, f)
	lazyTestSetup(t, m, clock, 5*time.Minute-time.Second)

	tok, _, err := m.Token(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, "t-second", tok)
	assert.EqualValues(t, 2, f.tokenHits.Load(), "tokenHits")
}

func TestLazy_SyncCallerReusesInFlightRefresh(t *testing.T) {
	f := newFakeFeishu(t)
	f.setTokens("t-first", "t-second")
	f.setTokenDelay(200 * time.Millisecond)
	m, clock, _ := newLazyTestManager(t, f)
	lazyTestSetup(t, m, clock, 20*time.Minute)
	ctx := context.Background()

	tok, _, err := m.Token(ctx)
	require.NoError(t, err)
	require.Equal(t, "t-first", tok, "Token that triggers async refresh")
	clock.Advance(16 * time.Minute)
	tok, _, err = m.Token(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "t-second", tok, "Token with 4min left")
	waitRefreshIdle(t, m)
	assert.EqualValues(t, 2, f.tokenHits.Load(), "tokenHits")
}

func TestLazy_AsyncRefreshIgnoresCallerCancel(t *testing.T) {
	f := newFakeFeishu(t)
	f.setTokens("t-first", "t-second")
	f.setTokenDelay(50 * time.Millisecond)
	m, clock, _ := newLazyTestManager(t, f)
	lazyTestSetup(t, m, clock, 20*time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tok, _, err := m.Token(ctx)
	assert.NoError(t, err)
	assert.Equal(t, "t-first", tok)
	waitRefreshIdle(t, m)
	assert.EqualValues(t, 2, f.tokenHits.Load(), "tokenHits")
	tok, _, err = m.Token(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, "t-second", tok, "Token after async refresh")
}
