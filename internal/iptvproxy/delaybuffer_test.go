package iptvproxy

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDelayedReader_HoldsDataForDelay(t *testing.T) {
	src := strings.NewReader("hello world")
	dr := newDelayedReader(context.Background(), src, 100*time.Millisecond)

	start := time.Now()
	out, err := io.ReadAll(dr)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Equal(t, "hello world", string(out))
	assert.GreaterOrEqual(t, elapsed, 90*time.Millisecond, "first byte should be held back for roughly the configured delay")
}

func TestDelayedReader_PropagatesUpstreamError(t *testing.T) {
	upstreamErr := io.ErrUnexpectedEOF
	src := &erroringReader{err: upstreamErr}
	dr := newDelayedReader(context.Background(), src, 10*time.Millisecond)

	_, err := io.ReadAll(dr)
	assert.ErrorIs(t, err, upstreamErr)
}

func TestDelayedReader_CanceledContextStopsRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// blockingReader never returns, so Read would hang forever without ctx.
	dr := newDelayedReader(ctx, &blockingReader{}, time.Hour)

	done := make(chan error, 1)
	go func() {
		_, err := dr.Read(make([]byte, 1))
		done <- err
	}()

	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("Read did not return after context cancellation")
	}
}

// erroringReader returns err on every Read after emitting no data.
type erroringReader struct{ err error }

func (r *erroringReader) Read([]byte) (int, error) { return 0, r.err }

// blockingReader never returns from Read until the caller gives up.
type blockingReader struct{}

func (r *blockingReader) Read([]byte) (int, error) {
	select {}
}
