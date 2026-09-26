package extractor

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

func TestFanOutBoundsWorkersAndPreservesOrder(t *testing.T) {
	items := make([]int, 2000)
	for i := range items {
		items[i] = i
	}
	started := make(chan struct{}, config.PageFetchConcurrency)
	release := make(chan struct{})
	result := make(chan []int, 1)
	before := runtime.NumGoroutine()
	go func() {
		result <- FanOut(context.Background(), items, func(_ context.Context, i int) ([]int, error) {
			if i < config.PageFetchConcurrency {
				started <- struct{}{}
			}
			<-release
			if i%7 == 0 {
				return nil, errors.New("unavailable")
			}
			return []int{i}, nil
		})
	}()
	for range config.PageFetchConcurrency {
		<-started
	}
	peak := runtime.NumGoroutine() - before
	close(release)
	got := <-result
	if peak > config.PageFetchConcurrency+2 {
		t.Errorf("launched %d goroutines for %d fetch slots", peak, config.PageFetchConcurrency)
	}
	var want []int
	for _, i := range items {
		if i%7 != 0 {
			want = append(want, i)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatal("results lost input order or included failed fetches")
	}
}

func TestFanOutDoesNotFetchAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := FanOut(ctx, []int{1, 2, 3}, func(context.Context, int) ([]int, error) {
		t.Error("called fetch after cancellation")
		return []int{1}, nil
	})
	if len(got) != 0 {
		t.Fatalf("returned results after cancellation: %v", got)
	}
}
