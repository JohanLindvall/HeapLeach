package download

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// contentRange describes bytes, with an inclusive last offset. A first of
// -1 means an unsatisfied range; a total of -1 means an unknown length.
type contentRange struct{ first, last, total int64 }

func parseContentRange(value string) (contentRange, bool) {
	r := contentRange{first: -1, last: -1, total: -1}
	unit, value, ok := strings.Cut(strings.TrimSpace(value), " ")
	if !ok || !strings.EqualFold(unit, "bytes") {
		return r, false
	}
	span, length, ok := strings.Cut(value, "/")
	if !ok {
		return r, false
	}
	if length != "*" {
		if r.total, ok = rangeNumber(length); !ok {
			return r, false
		}
	}
	if span == "*" {
		return r, r.total >= 0
	}
	first, last, ok := strings.Cut(span, "-")
	if !ok {
		return r, false
	}
	if r.first, ok = rangeNumber(first); !ok {
		return r, false
	}
	if r.last, ok = rangeNumber(last); !ok {
		return r, false
	}
	return r, r.last >= r.first && r.last < math.MaxInt64 && (r.total < 0 || r.last < r.total)
}

func rangeNumber(value string) (int64, bool) {
	if value == "" || value[0] < '0' || value[0] > '9' {
		return 0, false
	}
	n, err := strconv.ParseInt(value, 10, 64)
	return n, err == nil && n >= 0
}

// validatePartial checks the same invariants for primary and extra streams
// before any bytes can be combined with the part file. end is exclusive;
// negative end or total means the request did not constrain that value.
func validatePartial(resp *http.Response, start, end, total int64) error {
	value := resp.Header.Get(httpx.HeaderContentRange)
	r, ok := parseContentRange(value)
	if !ok || r.first < 0 || r.first != start || (end > 0 && r.last >= end) {
		return fmt.Errorf("invalid Content-Range %q for bytes %d-%d", value, start, end-1)
	}
	if total > 0 && r.total >= 0 && r.total != total {
		return errFileChanged
	}
	length := r.last - r.first + 1
	if resp.ContentLength >= 0 && resp.ContentLength != length {
		return fmt.Errorf("Content-Length %d disagrees with Content-Range %q", resp.ContentLength, value)
	}
	// Chunked responses have no Content-Length to check. Their body still
	// belongs only to the range declared in the header.
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.LimitReader(resp.Body, length), resp.Body}
	return nil
}

func parseContentRangeTotal(value string) (int64, bool) {
	r, ok := parseContentRange(value)
	return r.total, ok && r.total >= 0
}
