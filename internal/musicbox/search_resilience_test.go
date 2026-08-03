package musicbox

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSearchCoordinatorRetriesEmptyResponseAndCachesRecovery(t *testing.T) {
	var calls atomic.Int32
	coordinator := newTestSearchCoordinator(
		func(
			context.Context,
			string,
			string,
			int,
			int,
		) ([]SearchTrack, error) {
			if calls.Add(1) < 5 {
				return []SearchTrack{}, nil
			}
			return []SearchTrack{testSearchTrack("track_1", "晴天")}, nil
		},
	)

	results, err := coordinator.Search(
		context.Background(),
		"netease",
		"晴天",
		20,
		1,
	)
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) != 1 || results[0].Name != "晴天" {
		t.Fatalf("unexpected results: %#v", results)
	}
	if got := calls.Load(); got != 5 {
		t.Fatalf("backend calls = %d, want 5", got)
	}

	results[0].Name = "mutated"
	results[0].Artists[0] = "mutated"
	cached, err := coordinator.Search(
		context.Background(),
		" netease ",
		"  晴天  ",
		20,
		1,
	)
	if err != nil {
		t.Fatalf("cached Search returned error: %v", err)
	}
	if got := calls.Load(); got != 5 {
		t.Fatalf("cached backend calls = %d, want 5", got)
	}
	if cached[0].Name != "晴天" || cached[0].Artists[0] != "周杰伦" {
		t.Fatalf("cache was mutated through caller result: %#v", cached)
	}
}

func TestSearchCoordinatorCoalescesConcurrentRequests(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	coordinator := newTestSearchCoordinator(
		func(
			context.Context,
			string,
			string,
			int,
			int,
		) ([]SearchTrack, error) {
			if calls.Add(1) == 1 {
				close(started)
			}
			<-release
			return []SearchTrack{testSearchTrack("track_1", "晴天")}, nil
		},
	)

	const waiterCount = 12
	var wg sync.WaitGroup
	wg.Add(waiterCount)
	errs := make(chan error, waiterCount)
	for range waiterCount {
		go func() {
			defer wg.Done()
			results, err := coordinator.Search(
				context.Background(),
				"netease",
				"晴天",
				20,
				1,
			)
			if err != nil {
				errs <- err
				return
			}
			if len(results) != 1 || results[0].TrackID != "track_1" {
				errs <- errors.New("unexpected coalesced search result")
			}
		}()
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("backend search did not start")
	}
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("backend calls before release = %d, want 1", got)
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("backend calls = %d, want 1", got)
	}
}

func TestSearchCoordinatorFallsBackToStaleNonEmptyResult(t *testing.T) {
	now := time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	coordinator := newSearchCoordinatorWithPolicy(
		func(
			context.Context,
			string,
			string,
			int,
			int,
		) ([]SearchTrack, error) {
			if calls.Add(1) == 1 {
				return []SearchTrack{testSearchTrack("track_1", "晴天")}, nil
			}
			return []SearchTrack{}, nil
		},
		searchCoordinatorPolicy{
			freshTTL:   time.Minute,
			staleTTL:   10 * time.Minute,
			maxEntries: 16,
			timeout:    time.Second,
			retryDelay: []time.Duration{0, 0},
			now:        func() time.Time { return now },
			logf:       func(string, ...any) {},
		},
	)

	first, err := coordinator.Search(
		context.Background(),
		"netease",
		"晴天",
		20,
		1,
	)
	if err != nil || len(first) != 1 {
		t.Fatalf("initial Search = %#v, %v", first, err)
	}

	now = now.Add(2 * time.Minute)
	stale, err := coordinator.Search(
		context.Background(),
		"netease",
		"晴天",
		20,
		1,
	)
	if err != nil {
		t.Fatalf("stale Search returned error: %v", err)
	}
	if len(stale) != 1 || stale[0].TrackID != "track_1" {
		t.Fatalf("stale Search = %#v", stale)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("backend calls = %d, want 4", got)
	}
}

func TestSearchCoordinatorDoesNotCacheEmptyResults(t *testing.T) {
	var calls atomic.Int32
	coordinator := newTestSearchCoordinator(
		func(
			context.Context,
			string,
			string,
			int,
			int,
		) ([]SearchTrack, error) {
			calls.Add(1)
			return []SearchTrack{}, nil
		},
	)

	for range 2 {
		results, err := coordinator.Search(
			context.Background(),
			"bilibili",
			"unlikely-result",
			20,
			1,
		)
		if err != nil {
			t.Fatalf("Search returned error: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("Search results = %#v, want empty", results)
		}
	}
	if got := calls.Load(); got != 10 {
		t.Fatalf("backend calls = %d, want 10", got)
	}
}

func TestSearchCoordinatorReturnsStaleResultAfterBackendErrors(t *testing.T) {
	now := time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	coordinator := newSearchCoordinatorWithPolicy(
		func(
			context.Context,
			string,
			string,
			int,
			int,
		) ([]SearchTrack, error) {
			if calls.Add(1) == 1 {
				return []SearchTrack{testSearchTrack("track_1", "晴天")}, nil
			}
			return nil, errors.New("upstream unavailable")
		},
		searchCoordinatorPolicy{
			freshTTL:   time.Minute,
			staleTTL:   10 * time.Minute,
			maxEntries: 16,
			timeout:    time.Second,
			retryDelay: []time.Duration{0, 0},
			now:        func() time.Time { return now },
			logf:       func(string, ...any) {},
		},
	)

	if _, err := coordinator.Search(
		context.Background(),
		"netease",
		"晴天",
		20,
		1,
	); err != nil {
		t.Fatalf("initial Search returned error: %v", err)
	}
	now = now.Add(2 * time.Minute)

	results, err := coordinator.Search(
		context.Background(),
		"netease",
		"晴天",
		20,
		1,
	)
	if err != nil {
		t.Fatalf("fallback Search returned error: %v", err)
	}
	if len(results) != 1 || results[0].TrackID != "track_1" {
		t.Fatalf("fallback Search = %#v", results)
	}
}

func newTestSearchCoordinator(fetch searchFetchFunc) *searchCoordinator {
	return newSearchCoordinatorWithPolicy(fetch, searchCoordinatorPolicy{
		freshTTL:   time.Minute,
		staleTTL:   10 * time.Minute,
		maxEntries: 16,
		timeout:    time.Second,
		retryDelay: []time.Duration{0, 0, 0, 0},
		now:        time.Now,
		logf:       func(string, ...any) {},
	})
}

func testSearchTrack(id, name string) SearchTrack {
	return SearchTrack{
		TrackID: id,
		Name:    name,
		Artists: []string{"周杰伦"},
		Source:  "netease",
	}
}
