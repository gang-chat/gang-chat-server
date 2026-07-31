package musicbox

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

const (
	defaultSearchFreshTTL   = 2 * time.Minute
	defaultSearchStaleTTL   = 30 * time.Minute
	defaultSearchMaxEntries = 256
	defaultSearchTimeout    = 25 * time.Second
)

var defaultSearchRetryDelays = []time.Duration{
	150 * time.Millisecond,
	350 * time.Millisecond,
}

type searchFetchFunc func(
	ctx context.Context,
	source string,
	keyword string,
	count int,
	page int,
) ([]SearchTrack, error)

type searchCoordinatorPolicy struct {
	freshTTL   time.Duration
	staleTTL   time.Duration
	maxEntries int
	timeout    time.Duration
	retryDelay []time.Duration
	now        func() time.Time
	logf       func(format string, args ...any)
}

type searchCoordinator struct {
	fetch  searchFetchFunc
	policy searchCoordinatorPolicy

	mu       sync.Mutex
	cache    map[string]searchCacheEntry
	inFlight map[string]*searchCall
}

type searchCacheEntry struct {
	results    []SearchTrack
	freshUntil time.Time
	staleUntil time.Time
	storedAt   time.Time
}

type searchCall struct {
	done    chan struct{}
	results []SearchTrack
	err     error
}

type normalizedSearchRequest struct {
	key       string
	source    string
	keyword   string
	count     int
	page      int
	queryHash string
}

func newSearchCoordinator(fetch searchFetchFunc) *searchCoordinator {
	return newSearchCoordinatorWithPolicy(fetch, searchCoordinatorPolicy{
		freshTTL:   defaultSearchFreshTTL,
		staleTTL:   defaultSearchStaleTTL,
		maxEntries: defaultSearchMaxEntries,
		timeout:    defaultSearchTimeout,
		retryDelay: append([]time.Duration(nil), defaultSearchRetryDelays...),
		now:        time.Now,
		logf:       log.Printf,
	})
}

func newSearchCoordinatorWithPolicy(
	fetch searchFetchFunc,
	policy searchCoordinatorPolicy,
) *searchCoordinator {
	if policy.freshTTL <= 0 {
		policy.freshTTL = defaultSearchFreshTTL
	}
	if policy.staleTTL < policy.freshTTL {
		policy.staleTTL = policy.freshTTL
	}
	if policy.maxEntries <= 0 {
		policy.maxEntries = defaultSearchMaxEntries
	}
	if policy.timeout <= 0 {
		policy.timeout = defaultSearchTimeout
	}
	if policy.now == nil {
		policy.now = time.Now
	}
	if policy.logf == nil {
		policy.logf = func(string, ...any) {}
	}
	return &searchCoordinator{
		fetch:    fetch,
		policy:   policy,
		cache:    make(map[string]searchCacheEntry),
		inFlight: make(map[string]*searchCall),
	}
}

func (c *searchCoordinator) Search(
	ctx context.Context,
	source string,
	keyword string,
	count int,
	page int,
) ([]SearchTrack, error) {
	request := normalizeSearchRequest(source, keyword, count, page)
	now := c.policy.now()

	c.mu.Lock()
	if entry, ok := c.cache[request.key]; ok {
		if now.Before(entry.freshUntil) {
			results := cloneSearchTracks(entry.results)
			c.mu.Unlock()
			return results, nil
		}
		if !now.Before(entry.staleUntil) {
			delete(c.cache, request.key)
		}
	}
	if call, ok := c.inFlight[request.key]; ok {
		c.mu.Unlock()
		return waitForSearchCall(ctx, call)
	}
	call := &searchCall{done: make(chan struct{})}
	c.inFlight[request.key] = call
	c.mu.Unlock()

	go c.run(call, request)
	return waitForSearchCall(ctx, call)
}

func (c *searchCoordinator) run(
	call *searchCall,
	request normalizedSearchRequest,
) {
	ctx, cancel := context.WithTimeout(context.Background(), c.policy.timeout)
	defer cancel()

	var (
		results []SearchTrack
		lastErr error
	)
	attempts := len(c.policy.retryDelay) + 1
	for attempt := 1; attempt <= attempts; attempt++ {
		startedAt := time.Now()
		results, lastErr = c.fetch(
			ctx,
			request.source,
			request.keyword,
			request.count,
			request.page,
		)
		elapsed := time.Since(startedAt)
		if lastErr == nil && len(results) > 0 {
			if attempt > 1 {
				c.policy.logf(
					"musicbox search recovered source=%q query_hash=%s attempt=%d/%d results=%d latency_ms=%d",
					request.source,
					request.queryHash,
					attempt,
					attempts,
					len(results),
					elapsed.Milliseconds(),
				)
			}
			break
		}
		if lastErr == nil {
			c.policy.logf(
				"musicbox search empty source=%q query_hash=%s attempt=%d/%d latency_ms=%d",
				request.source,
				request.queryHash,
				attempt,
				attempts,
				elapsed.Milliseconds(),
			)
		} else {
			c.policy.logf(
				"musicbox search failed source=%q query_hash=%s attempt=%d/%d latency_ms=%d error=%q",
				request.source,
				request.queryHash,
				attempt,
				attempts,
				elapsed.Milliseconds(),
				lastErr.Error(),
			)
		}
		if attempt == attempts {
			break
		}
		if !waitSearchRetry(ctx, c.policy.retryDelay[attempt-1]) {
			lastErr = ctx.Err()
			break
		}
	}

	now := c.policy.now()
	c.mu.Lock()
	if len(results) > 0 && lastErr == nil {
		results = cloneSearchTracks(results)
		c.storeLocked(request.key, results, now)
	} else if entry, ok := c.cache[request.key]; ok &&
		now.Before(entry.staleUntil) &&
		len(entry.results) > 0 {
		results = cloneSearchTracks(entry.results)
		lastErr = nil
		c.policy.logf(
			"musicbox search served stale cache source=%q query_hash=%s results=%d",
			request.source,
			request.queryHash,
			len(results),
		)
	} else if lastErr == nil {
		results = []SearchTrack{}
	}
	call.results = cloneSearchTracks(results)
	call.err = lastErr
	delete(c.inFlight, request.key)
	close(call.done)
	c.mu.Unlock()
}

func (c *searchCoordinator) storeLocked(
	key string,
	results []SearchTrack,
	now time.Time,
) {
	if _, exists := c.cache[key]; !exists && len(c.cache) >= c.policy.maxEntries {
		var (
			oldestKey string
			oldestAt  time.Time
		)
		for candidateKey, entry := range c.cache {
			if oldestKey == "" || entry.storedAt.Before(oldestAt) {
				oldestKey = candidateKey
				oldestAt = entry.storedAt
			}
		}
		if oldestKey != "" {
			delete(c.cache, oldestKey)
		}
	}
	c.cache[key] = searchCacheEntry{
		results:    cloneSearchTracks(results),
		freshUntil: now.Add(c.policy.freshTTL),
		staleUntil: now.Add(c.policy.staleTTL),
		storedAt:   now,
	}
}

func normalizeSearchRequest(
	source string,
	keyword string,
	count int,
	page int,
) normalizedSearchRequest {
	normalizedSource := strings.TrimSpace(source)
	if normalizedSource == "" {
		normalizedSource = defaultGDSource
	}
	normalizedKeyword := strings.Join(strings.Fields(keyword), " ")
	if count <= 0 {
		count = 20
	}
	if page <= 0 {
		page = 1
	}
	cacheKeyword := strings.ToLower(normalizedKeyword)
	hash := sha256.Sum256([]byte(cacheKeyword))
	return normalizedSearchRequest{
		key: fmt.Sprintf(
			"%s\x00%s\x00%d\x00%d",
			normalizedSource,
			cacheKeyword,
			count,
			page,
		),
		source:    normalizedSource,
		keyword:   normalizedKeyword,
		count:     count,
		page:      page,
		queryHash: fmt.Sprintf("%x", hash[:6]),
	}
}

func waitForSearchCall(
	ctx context.Context,
	call *searchCall,
) ([]SearchTrack, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-call.done:
		return cloneSearchTracks(call.results), call.err
	}
}

func waitSearchRetry(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func cloneSearchTracks(values []SearchTrack) []SearchTrack {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]SearchTrack, len(values))
	for index, value := range values {
		cloned[index] = value
		cloned[index].Artists = append([]string(nil), value.Artists...)
	}
	return cloned
}
