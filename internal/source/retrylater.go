package source

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// QueueFullError is the exact "error" value the API puts in the body of a 429
// refused by its installer download queue - the discriminant between that
// ("the shared pool of download slots is full, come back later") and a 429
// from one of its per-IP rate limiters, which carries no such body.
const QueueFullError = "download queue full"

// DefaultRetryAfter is the wait used when a 429 names none the client can
// use: no Retry-After header, no retry_after body field, or only values that
// are non-numeric or <= 0.
const DefaultRetryAfter = 60 * time.Second

// RetryLaterError reports that the server refused a setup download with HTTP
// 429. It is a "come back later", not an outage: download.Manager waits Wait
// (plus its own jitter) before asking again, and never sooner.
type RetryLaterError struct {
	// Wait is what the server asked for, without jitter - see retryAfter.
	Wait time.Duration
	// QueueFull is true when the body said QueueFullError: the download
	// queue is full, as opposed to a rate limiter.
	QueueFull bool
	// Message is the body's human-readable text, for the log only.
	Message string
	// Capacity and Active describe the queue at refusal time. Informational
	// only - nothing may decide anything on them.
	Capacity, Active int
}

func (e *RetryLaterError) Error() string {
	if e.QueueFull {
		return fmt.Sprintf("setup download refused: %s (retry after %s)", QueueFullError, e.Wait)
	}
	return fmt.Sprintf("setup download returned HTTP 429 (retry after %s)", e.Wait)
}

// retryLaterFrom builds the RetryLaterError for a 429 response. A body that
// is missing or not the queue's JSON is tolerated: that is exactly what a
// rate limiter's 429 looks like.
func retryLaterFrom(resp *http.Response) *RetryLaterError {
	var body struct {
		Error      string `json:"error"`
		Message    string `json:"message"`
		RetryAfter int    `json:"retry_after"`
		Capacity   int    `json:"capacity"`
		Active     int    `json:"active"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body)

	return &RetryLaterError{
		Wait:      retryAfter(resp.Header.Get("Retry-After"), body.RetryAfter),
		QueueFull: body.Error == QueueFullError,
		Message:   body.Message,
		Capacity:  body.Capacity,
		Active:    body.Active,
	}
}

// retryAfter picks the wait a 429 asks for: the Retry-After header (integer
// seconds), else the body's retry_after, else DefaultRetryAfter. A value that
// is not a positive integer is skipped - an HTTP-date Retry-After included,
// which the API never sends.
func retryAfter(header string, bodySeconds int) time.Duration {
	if s, err := strconv.Atoi(header); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if bodySeconds > 0 {
		return time.Duration(bodySeconds) * time.Second
	}
	return DefaultRetryAfter
}
