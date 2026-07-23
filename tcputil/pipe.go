package tcputil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

type closeWriter interface {
	CloseWrite() error
}

type copyResult struct {
	direction string
	err       error
}

const defaultHalfCloseTimeout = time.Second

// Pipe copies data in both directions while preserving TCP-style half-close
// semantics. Both production endpoints (TCP and smux streams) implement
// CloseWrite. If a different connection cannot half-close, Pipe aborts the
// other direction instead of waiting forever.
func Pipe(ctx context.Context, left, right net.Conn) error {
	return pipeWithHalfCloseTimeout(ctx, left, right, defaultHalfCloseTimeout)
}

func pipeWithHalfCloseTimeout(
	ctx context.Context,
	left, right net.Conn,
	halfCloseTimeout time.Duration,
) error {
	copyCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	deadlineDone := make(chan struct{})
	stopDeadline := context.AfterFunc(copyCtx, func() {
		defer close(deadlineDone)
		deadline := time.Now()
		_ = left.SetDeadline(deadline)
		_ = right.SetDeadline(deadline)
	})

	results := make(chan copyResult, 2)
	copyDirection := func(direction string, destination, source net.Conn) {
		_, err := io.Copy(destination, source)
		if err == nil {
			writer, ok := destination.(closeWriter)
			if !ok {
				err = fmt.Errorf("%s: destination %T does not support CloseWrite", direction, destination)
			} else if closeErr := closeWriteBounded(copyCtx, destination, writer, halfCloseTimeout); closeErr != nil &&
				!errors.Is(closeErr, net.ErrClosed) {
				err = fmt.Errorf("%s: close write: %w", direction, closeErr)
			}
		} else {
			err = fmt.Errorf("%s: %w", direction, err)
		}
		results <- copyResult{direction: direction, err: err}
	}

	go copyDirection("left<-right", left, right)
	go copyDirection("right<-left", right, left)

	var firstErr error
	remaining := 2
	for remaining > 0 {
		select {
		case result := <-results:
			remaining--
			if result.err != nil && firstErr == nil && ctx.Err() == nil {
				firstErr = result.err
				cancel()
			}
		case <-ctx.Done():
			if firstErr == nil {
				firstErr = ctx.Err()
			}
			cancel()
		}
	}

	cancel()
	if !stopDeadline() {
		<-deadlineDone
	}
	_ = left.SetDeadline(time.Time{})
	_ = right.SetDeadline(time.Time{})
	return firstErr
}

func closeWriteBounded(
	ctx context.Context,
	conn net.Conn,
	writer closeWriter,
	timeout time.Duration,
) error {
	if timeout <= 0 {
		_ = conn.Close()
		return fmt.Errorf("half-close timeout must be positive")
	}
	result := make(chan error, 1)
	go func() {
		result <- writer.CloseWrite()
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = conn.Close()
		return ctx.Err()
	case <-timer.C:
		// smux CloseWrite has its own long protocol timeout and does not honor
		// net.Conn deadlines. Fully closing the endpoint evicts that session and
		// unblocks the opposite copy direction.
		_ = conn.Close()
		return fmt.Errorf("timed out after %s", timeout)
	}
}
