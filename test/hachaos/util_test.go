//go:build linux

package hachaos

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

func freePort() (int, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return portOf(l.Addr())
}

func portOf(addr net.Addr) (int, error) {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("%s is not a TCP address", addr)
	}
	return tcp.Port, nil
}

type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

// errPermanent stops waitFor from retrying.
func errPermanent(err error) error { return permanentError{err} }

// waitFor retries fn until it succeeds, fails permanently, or timeout passes.
func waitFor(ctx context.Context, timeout time.Duration, fn func() error) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		last = fn()
		if last == nil {
			return nil
		}
		var permanent permanentError
		if errors.As(last, &permanent) {
			return permanent.error
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s: %w", timeout, last)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %w)", ctx.Err(), last)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
