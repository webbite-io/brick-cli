package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// --- Retrying transfers through network hiccups and Storage API restarts ---
//
// A streaming upload or download that fails on the way (a dropped or stalled
// connection, or the Storage API answering 502/503/504 while it restarts) is
// tried again with backoff rather than failing the transfer. A download picks
// up where it stopped (see resumableDownload); an upload starts over, since
// POST /files can't append to what it already received. A transfer gives up
// once transferRetryWindow passes without it getting any further than it had
// been, so a server that stays down still ends it as failed.

// Retry timing. Variables so tests can shorten them.
var (
	// transferRetryMin and transferRetryMax bound the wait between attempts,
	// which doubles from the one to the other and starts over after an
	// attempt that got further.
	transferRetryMin = 1 * time.Second
	transferRetryMax = 30 * time.Second
	// transferRetryWindow is how long a transfer keeps retrying without
	// getting further before it fails.
	transferRetryWindow = 10 * time.Minute
	// transferStallTimeout cuts an attempt off when no bytes have moved for
	// this long: a server that went away without closing the connection
	// would otherwise leave it hanging.
	transferStallTimeout = 2 * time.Minute
)

var errTransferStalled = errors.New("the connection stalled")

// storageStatusError is a Storage API answer with an unexpected status, as
// errFrom reports it.
type storageStatusError struct {
	code   int
	status string
	body   string
}

func (e *storageStatusError) Error() string {
	return fmt.Sprintf("storage API %s: %s", e.status, e.body)
}

// transientError marks err as worth retrying, for a failure whose status
// only its caller can judge (e.g. a source device's answer relayed by the
// Storage API).
type transientError struct{ err error }

func (e *transientError) Error() string { return e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }

// retryableStatus reports whether an HTTP status means the server is
// briefly unable to answer: overloaded, restarting, or behind a proxy that
// lost it. A 500 is left out, as it usually fails the same way again.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// isTransient reports whether a transfer that failed with err may succeed
// if tried again: the network or server failed, not the request or the
// local disk.
func isTransient(err error) bool {
	if errors.Is(err, errSessionExpired) {
		return false
	}
	var te *transientError
	if errors.As(err, &te) || errors.Is(err, errTransferStalled) {
		return true
	}
	var se *storageStatusError
	if errors.As(err, &se) {
		return retryableStatus(se.code)
	}
	// http.Client wraps every failure to send a request or read its
	// response's headers in a *url.Error.
	var ue *url.Error
	var ne net.Error
	return errors.As(err, &ue) || errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF)
}

// transferRetryObserver is told about each retry of a transfer: the error
// the attempt failed with and how long until the next one.
type transferRetryObserver func(err error, wait time.Duration)

type transferRetryObserverKey struct{}

// withTransferRetryObserver returns ctx carrying fn, which retryTransfer
// calls on every retry of a transfer run under it.
func withTransferRetryObserver(ctx context.Context, fn transferRetryObserver) context.Context {
	return context.WithValue(ctx, transferRetryObserverKey{}, fn)
}

// transferAttempt is one try at a transfer, run under ctx, which is cut off
// if touch isn't called for transferStallTimeout. It reports whether it got
// further than any attempt before it.
type transferAttempt func(ctx context.Context, touch func()) (progressed bool, err error)

// retryTransfer runs attempt until it succeeds, fails with an error that
// isn't transient, ctx ends, or transferRetryWindow passes without an
// attempt getting further. It returns the last attempt's error.
func retryTransfer(ctx context.Context, attempt transferAttempt) error {
	observe, _ := ctx.Value(transferRetryObserverKey{}).(transferRetryObserver)
	wait := transferRetryMin
	giveUpAt := time.Now().Add(transferRetryWindow)
	for {
		progressed, err := runGuarded(ctx, attempt)
		if err == nil || ctx.Err() != nil || !isTransient(err) {
			return err
		}
		if progressed {
			wait = transferRetryMin
			giveUpAt = time.Now().Add(transferRetryWindow)
		}
		if time.Now().Add(wait).After(giveUpAt) {
			return fmt.Errorf("gave up after retrying for %s: %w", transferRetryWindow, err)
		}
		if observe != nil {
			observe(err, wait)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
		wait = min(wait*2, transferRetryMax)
	}
}

// runGuarded runs one attempt, cancelling it if it stalls. An attempt cut
// off that way fails with errTransferStalled, whatever error the
// cancellation surfaced as.
func runGuarded(ctx context.Context, attempt transferAttempt) (bool, error) {
	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := time.AfterFunc(transferStallTimeout, func() { cancel(errTransferStalled) })
	defer timer.Stop()
	progressed, err := attempt(actx, func() { timer.Reset(transferStallTimeout) })
	if err != nil && errors.Is(context.Cause(actx), errTransferStalled) && ctx.Err() == nil {
		err = fmt.Errorf("%w after %s without data", errTransferStalled, transferStallTimeout)
	}
	return progressed, err
}

// resumableDownload streams what fetch serves to destPath, via a temporary
// file renamed into place once it's all there, reporting bytes written as it
// goes. An attempt that fails part way is retried (see retryTransfer) asking
// for only the rest, with Range. If-Range, set to the first response's
// validator, makes a file that changed in the meantime come back whole, and
// the download then starts over. So does a server that ignores Range.
//
// fetch makes the request with the given extra headers. errFor turns a
// response with an unexpected status into the error to fail (or retry) with.
func resumableDownload(ctx context.Context, destPath string, total int64, onProgress func(written, total int64),
	fetch func(ctx context.Context, headers map[string]string) (*http.Response, error),
	errFor func(resp *http.Response) error) error {
	tmpPath := destPath + tmpSuffix
	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	var written int64
	var validator string // the ETag or Last-Modified of what's in out
	report := func() {
		if onProgress != nil {
			onProgress(written, total)
		}
	}
	restart := func() error {
		written, validator = 0, ""
		if err := out.Truncate(0); err != nil {
			return err
		}
		_, err := out.Seek(0, io.SeekStart)
		report()
		return err
	}

	err = retryTransfer(ctx, func(ctx context.Context, touch func()) (bool, error) {
		if written > 0 && written == total {
			return false, nil // the last attempt got every byte but failed before seeing the end
		}
		headers := map[string]string{}
		if written > 0 && validator != "" {
			headers["Range"] = fmt.Sprintf("bytes=%d-", written)
			headers["If-Range"] = validator
		}
		resp, err := fetch(ctx, headers)
		if err != nil {
			return false, err
		}
		defer resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK:
			if written > 0 {
				if err := restart(); err != nil {
					return false, err
				}
			}
			validator = responseValidator(resp)
		case http.StatusPartialContent:
			if _, ok := headers["Range"]; !ok || contentRangeStart(resp) != written {
				if err := restart(); err != nil {
					return false, err
				}
				return false, &transientError{fmt.Errorf("unexpected partial response %q", resp.Header.Get("Content-Range"))}
			}
		default:
			return false, errFor(resp)
		}

		start := written
		buf := make([]byte, 256*1024)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				touch()
				if _, err := out.Write(buf[:n]); err != nil {
					return written > start, err
				}
				written += int64(n)
				report()
			}
			if readErr == io.EOF {
				return written > start, nil
			}
			if readErr != nil {
				return written > start, &transientError{readErr}
			}
		}
	})
	if err == nil {
		err = out.Close()
	} else {
		out.Close()
	}
	if err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// responseValidator returns what an If-Range for the rest of resp's body
// should name: its strong ETag, else its Last-Modified, else "" (no resuming).
func responseValidator(resp *http.Response) string {
	if etag := resp.Header.Get("ETag"); etag != "" && !strings.HasPrefix(etag, "W/") {
		return etag
	}
	return resp.Header.Get("Last-Modified")
}

// contentRangeStart returns the first byte position of a 206 response's
// Content-Range ("bytes 100-199/200"), or -1 if it can't be read.
func contentRangeStart(resp *http.Response) int64 {
	var start, end, size int64
	if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &size); err != nil {
		return -1
	}
	return start
}
