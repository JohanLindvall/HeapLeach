// SPDX-License-Identifier: MIT

package extractor

import (
	"errors"
	"fmt"
	"time"
)

// WaitError is a resolver's way of saying a file cannot be fetched yet,
// rather than that it failed: Keep2Share, say, makes an address wait the
// best part of an hour between free downloads. The downloader puts the item
// back in the queue until Until, as waiting rather than downloading, and the
// worker it held goes on to something else meanwhile. Resolve is called
// again once the time has come.
type WaitError struct {
	Until time.Time
	// Reason is shown while the item waits, followed by the time left.
	Reason string
}

func (e *WaitError) Error() string {
	return fmt.Sprintf("%s until %s", e.Reason, e.Until.Format(time.TimeOnly))
}

// RefusedError is a resolver's way of saying a host refused the address it
// was asked from, not the file. Keep2Share answers "Download is not
// available", and nothing more, to free downloads from some addresses while
// handing the same file to the next one. Through a proxy route the
// downloader holds that route back from the host for a while and tries the
// file through another; without one there is nowhere else to ask, and it is
// an ordinary failure.
type RefusedError struct{ Err error }

func (e *RefusedError) Error() string { return e.Err.Error() }
func (e *RefusedError) Unwrap() error { return e.Err }

// TransientError marks a resolver's failure that the next attempt may well
// not meet, though nothing below HTTP saw it fail: a run of CAPTCHAs that
// could not be read in time, say. Through a proxy route the downloader
// tries the file again on the best route free; without one, resolution
// retries with backoff up to the normal retry budget.
type TransientError struct{ Err error }

func (e *TransientError) Error() string { return e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// ErrPasswordRequired signals that the content is gated behind a password.
var ErrPasswordRequired = errors.New("this link is password protected: supply the password and retry")
