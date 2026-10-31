// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// TestResolveFilesKeepsOrderAndReasons pins the multi-file resolve: each
// item's files in the listing's order, whichever finished first, the
// refusals in order beside them, and the first other failure.
func TestResolveListingKeepsOrderAndReasons(t *testing.T) {
	resolve := func(_ context.Context, item string) ([]File, error) {
		switch item {
		case "refused-1", "refused-2":
			return nil, &itemRefusal{"site", item, "withheld " + item}
		case "failed-1", "failed-2":
			return nil, errors.New("site: " + item)
		case "empty":
			return nil, nil
		}
		return []File{{Name: item + "-a"}, {Name: item + "-b"}}, nil
	}
	items := []string{"one", "failed-1", "refused-1", "empty", "two", "refused-2", "failed-2"}
	files, s := resolveListing(context.Background(), items, resolve)
	refusals, failed := s.refusals, s.failed
	if got := fmt.Sprint(fileNames(files)); got != "[one-a one-b two-a two-b]" {
		t.Errorf("files = %s", got)
	}
	if got := fmt.Sprint(refusals); got != "[withheld refused-1 withheld refused-2]" {
		t.Errorf("refusals = %s", got)
	}
	if failed == nil || failed.Error() != "site: failed-1" {
		t.Errorf("failed = %v", failed)
	}
	if s.listed != 7 || s.succeeded != 2 || s.empty != 1 || s.failures != 2 {
		t.Errorf("summary = %+v", s)
	}
}

// TestNoneReadSaysWhy pins the error a listing gives when none of its
// items could be read: a refusal first, then a failure less the site's
// name, then that they held nothing.
func TestNoneReadSaysWhy(t *testing.T) {
	for _, tc := range []struct {
		refusals []string
		failed   error
		want     string
	}{
		{[]string{"it is private"}, errors.New("site: transport"), "site: none of the 3 releases on the link could be read: it is private"},
		{nil, errors.New("site: transport"), "site: none of the 3 releases on the link could be read: transport"},
		{nil, nil, "site: none of the 3 releases on the link could be read: they hold nothing to download"},
	} {
		if got := noneRead("site", 3, "releases", "the link", tc.refusals, tc.failed).Error(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

// Both modes of the ordered map stop the same way: nothing starts once the
// context has ended, a cancellation during a callback stops the items
// after it, and the outcomes that did arrive keep the listing's order.
func TestResolveOutcomesStopsAtCancellation(t *testing.T) {
	items := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19}
	for _, inTurn := range []bool{true, false} {
		t.Run(fmt.Sprintf("inTurn=%v", inTurn), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var calls atomic.Int32
			got := resolveOutcomes(ctx, items, inTurn, func(context.Context, int) (int, error) {
				calls.Add(1)
				return 0, nil
			})
			if len(got) != 0 || calls.Load() != 0 {
				t.Fatalf("cancelled before work: %d outcomes, %d calls", len(got), calls.Load())
			}

			ctx, cancel = context.WithCancel(context.Background())
			defer cancel()
			calls.Store(0)
			got = resolveOutcomes(ctx, items, inTurn, func(ctx context.Context, i int) (int, error) {
				calls.Add(1)
				switch {
				case i == 2:
					cancel()
				case !inTurn:
					// Held until the cancellation, so that the workers
					// cannot race through the listing before item 2's
					// worker is scheduled: what is measured is what
					// starts after the context ends.
					<-ctx.Done()
				}
				return i * 10, nil
			})
			if inTurn && (calls.Load() != 3 || len(got) != 3) {
				t.Errorf("serial callbacks after cancellation: %d calls, %d outcomes", calls.Load(), len(got))
			}
			if !inTurn && int(calls.Load()) > 2+config.PageFetchConcurrency {
				t.Errorf("concurrent map kept starting items: %d calls", calls.Load())
			}
			for k := 1; k < len(got); k++ {
				if got[k].index <= got[k-1].index || got[k].got != got[k].index*10 || items[got[k].index] != got[k].item {
					t.Fatalf("outcomes out of order: %+v", got)
				}
			}
		})
	}
}

// The concurrent map stays within config.PageFetchConcurrency.
func TestResolveOutcomesIsBounded(t *testing.T) {
	items := make([]int, 4*config.PageFetchConcurrency)
	var active, peak atomic.Int32
	resolveOutcomes(context.Background(), items, false, func(context.Context, int) (int, error) {
		n := active.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(2 * time.Millisecond)
		active.Add(-1)
		return 0, nil
	})
	if peak.Load() > int32(config.PageFetchConcurrency) {
		t.Errorf("peak concurrency %d", peak.Load())
	}
}

// A listing cancelled after some of its items resolved returns the
// cancellation, never the partial listing as a success; one that is not
// cancelled keeps its files in order and says why when none resolved.
func TestListingFinishersPreferCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	files, _, err := readListing(ctx, "site", "posts", "a listing", []string{"a", "b"},
		func(_ context.Context, item string) ([]File, error) {
			cancel()
			return []File{{Name: item}}, nil
		})
	if !errors.Is(err, context.Canceled) || files != nil {
		t.Fatalf("cancelled listing: files=%v err=%v", files, err)
	}
	if err := noneResolved(ctx, []File{{Name: "a"}}, "site", 1, "posts", "x", nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("noneResolved with files after cancellation = %v", err)
	}

	files, note, err := readListing(context.Background(), "site", "posts", "a listing", []string{"a", "b", "c"},
		func(_ context.Context, item string) ([]File, error) {
			if item == "b" {
				return nil, &itemRefusal{site: "site", reason: "it is gone"}
			}
			return []File{{Name: item}}, nil
		})
	if err != nil || len(files) != 2 || files[0].Name != "a" || files[1].Name != "c" || note == "" {
		t.Fatalf("mixed listing: files=%v note=%q err=%v", files, note, err)
	}
	_, _, err = readListing(context.Background(), "site", "posts", "a listing", []string{"a"},
		func(context.Context, string) ([]File, error) { return nil, errors.New("site: broken") })
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("all failed: %v", err)
	}
}

// A series cancelled while its episodes resolve returns the cancellation,
// through expandSeries as through listSeries.
func TestExpandSeriesPrefersCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := series[string]{site: "site", noun: "episodes", link: "x", items: []string{"a", "b"}, inTurn: true,
		resolve: func(_ context.Context, item string) (*File, error) {
			cancel()
			return &File{Name: item}, nil
		}}
	if resolved, _, err := expandSeries(ctx, s); !errors.Is(err, context.Canceled) || resolved != nil {
		t.Fatalf("expandSeries: %v, %v", resolved, err)
	}
}

// A season read in turn is not asked after a cancellation, and seasons
// read concurrently keep their order.
func TestReadSeasonsStopsAtCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	list, _ := readSeasons(ctx, []string{"1", "2", "3"}, true, func(_ context.Context, s string) (seasonList[string], error) {
		calls.Add(1)
		cancel()
		return seasonList[string]{items: []string{s}}, nil
	})
	if calls.Load() != 1 || len(list.items) != 1 {
		t.Fatalf("calls=%d items=%v", calls.Load(), list.items)
	}
	list, first := readSeasons(context.Background(), []string{"1", "2", "3"}, false, func(_ context.Context, s string) (seasonList[string], error) {
		if s == "1" {
			return seasonList[string]{}, errors.New("unreadable")
		}
		return seasonList[string]{items: []string{s, s + "b"}}, nil
	})
	if first == nil || !reflect.DeepEqual(list.items, []string{"2", "2b", "3", "3b"}) || list.note != unreadableSeasons {
		t.Fatalf("items=%v note=%q first=%v", list.items, list.note, first)
	}
}

// A listing some of whose items resolved says what it left out: refusals per
// reason, then the items that could not be read, whether a request or a
// parse failed. An item that resolved to nothing is the item as it is and
// goes unmentioned. When nothing resolved the error says why, a refusal
// outranking a failure; a cancellation outranks everything. Files keep the
// listing's order throughout.
func TestReadListingAccountsForEveryItem(t *testing.T) {
	network := fmt.Errorf("site: fetch: %w", context.DeadlineExceeded)
	parse := errors.New("site: post has no attachments block")
	refusal := &itemRefusal{site: "site", reason: "it is gone"}
	resolve := func(_ context.Context, item string) ([]File, error) {
		switch item {
		case "network":
			return nil, network
		case "parse":
			return nil, parse
		case "refused":
			return nil, refusal
		case "empty":
			return nil, nil
		}
		return []File{{Name: item + "1"}, {Name: item + "2"}}, nil
	}
	for _, tc := range []struct {
		name  string
		items []string
		files string
		note  string
		err   string
	}{
		{"all resolve", []string{"a", "b"}, "[a1 a2 b1 b2]", "", ""},
		{"network failure", []string{"a", "network", "b"}, "[a1 a2 b1 b2]", "1 left out: could not be read", ""},
		{"parse failure", []string{"parse", "a"}, "[a1 a2]", "1 left out: could not be read", ""},
		{"refusal", []string{"a", "refused"}, "[a1 a2]", "1 left out: it is gone", ""},
		{"refusals and failures", []string{"refused", "network", "a", "parse", "refused"}, "[a1 a2]",
			"2 left out: it is gone; 2 left out: could not be read", ""},
		{"empty item", []string{"a", "empty"}, "[a1 a2]", "", ""},
		{"only empty", []string{"empty"}, "[]", "",
			"site: none of the 1 posts on a listing could be read: they hold nothing to download"},
		{"all failed", []string{"network", "parse"}, "[]", "",
			"site: none of the 2 posts on a listing could be read: fetch: context deadline exceeded"},
		{"refusal outranks failure", []string{"parse", "refused"}, "[]", "",
			"site: none of the 2 posts on a listing could be read: it is gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, note, err := readListing(context.Background(), "site", "posts", "a listing", tc.items, resolve)
			if fmt.Sprint(err) != cmpOrNil(tc.err) {
				t.Fatalf("err = %v, want %s", err, tc.err)
			}
			if got := fmt.Sprint(fileNames(files)); got != tc.files || note != tc.note {
				t.Errorf("files %s, note %q; want %s, %q", got, note, tc.files, tc.note)
			}
		})
	}

	// Cancelled after some items resolved: the cancellation, not a listing.
	ctx, cancel := context.WithCancel(context.Background())
	files, note, err := readListing(ctx, "site", "posts", "a listing", []string{"a", "network", "b"},
		func(ctx context.Context, item string) ([]File, error) {
			defer cancel()
			return resolve(ctx, item)
		})
	if !errors.Is(err, context.Canceled) || files != nil || note != "" {
		t.Errorf("cancelled: files %v, note %q, err %v", files, note, err)
	}
}

// cmpOrNil is want as fmt prints an error, "<nil>" when there is none.
func cmpOrNil(want string) string {
	if want == "" {
		return "<nil>"
	}
	return want
}

// The account keeps refused, failed, empty, unrecognised and unstarted items
// apart, counting items rather than files.
func TestSummariseCountsItems(t *testing.T) {
	outcomes := []outcome[string, []File]{
		{index: 0, got: []File{{}, {}, {}}},
		{index: 1},
		{index: 2, err: &itemRefusal{site: "s", reason: "live"}},
		{index: 3, err: fmt.Errorf("s: %w", errUnrecognised)},
		{index: 4, err: errors.New("first")},
		{index: 5, err: errors.New("second")},
	}
	s := summarise(8, outcomes, func(got []File) bool { return len(got) > 0 })
	want := outcomeSummary{listed: 8, attempted: 6, succeeded: 1, empty: 1, failures: 2, unrecognised: 1,
		refusals: []string{"live"}, failed: outcomes[4].err}
	if !reflect.DeepEqual(s, want) || s.notStarted() != 2 {
		t.Errorf("summary = %+v (not started %d)", s, s.notStarted())
	}
}

// TestNestedListingsMultiplyTheirBound measures what nested expansion asks
// at once today: the bound holds per map, not per extraction, so a listing
// whose items are listings reaches PageFetchConcurrency² leaves in flight,
// and each concurrent root job multiplies that again. The maps bound
// callbacks, not requests: TestNestedDiscoveryRequestsInFlight measures the
// same nesting at the HTTP boundary.
func TestNestedListingsMultiplyTheirBound(t *testing.T) {
	c := config.PageFetchConcurrency
	for _, roots := range []int{1, 2} {
		t.Run(fmt.Sprint(roots, " roots"), func(t *testing.T) {
			want := int32(roots * c * c)
			var active, peak, total atomic.Int32
			reached := make(chan struct{})
			var once atomic.Bool
			leaf := func(context.Context, int) (int, error) {
				total.Add(1)
				n := active.Add(1)
				for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
				}
				if n == want && once.CompareAndSwap(false, true) {
					close(reached)
				}
				// Held until every leaf the bounds allow is in flight, or a
				// second has passed: the peak is then exactly what the
				// bounds permit rather than what the scheduler managed.
				select {
				case <-reached:
				case <-time.After(time.Second):
				}
				active.Add(-1)
				return 0, nil
			}
			listing := func(ctx context.Context, _ int) (int, error) {
				resolveOutcomes(ctx, make([]int, c), false, leaf)
				return 0, nil
			}
			resolveOutcomes(context.Background(), make([]int, roots), false, func(ctx context.Context, _ int) (int, error) {
				resolveOutcomes(ctx, make([]int, c), false, listing)
				return 0, nil
			})
			t.Logf("%d root jobs: peak %d leaves in flight, %d leaves in all", roots, peak.Load(), total.Load())
			if peak.Load() != want || total.Load() != want {
				t.Errorf("peak %d of %d leaves, want %d", peak.Load(), total.Load(), want)
			}
		})
	}
}
