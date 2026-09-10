package iptvproxy

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The delay ramps from ~0 up to the configured delay over the first delay's
// worth of wall-clock time (see delayedReader's doc comment), so a chunk
// read right at the start of the stream should come back almost
// immediately, not held for the full configured delay.
func TestDelayedReader_RampsUpDelayFromNearZero(t *testing.T) {
	pr, pw := io.Pipe()
	dr := newDelayedReader(context.Background(), pr, 200*time.Millisecond)
	go pw.Write([]byte("first"))

	start := time.Now()
	buf := make([]byte, len("first"))
	_, err := io.ReadFull(dr, buf)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Equal(t, "first", string(buf))
	assert.Less(t, elapsed, 100*time.Millisecond, "a chunk read right after start should ramp toward ~0 delay, not the full configured delay")
}

// Once the ramp period has fully elapsed, later chunks should be held for
// the full configured delay.
func TestDelayedReader_HoldsFullDelayAfterRampUp(t *testing.T) {
	delay := 100 * time.Millisecond
	pr, pw := io.Pipe()
	dr := newDelayedReader(context.Background(), pr, delay)

	writtenAt := make(chan time.Time, 1)
	go func() {
		time.Sleep(delay + 50*time.Millisecond) // let the ramp fully elapse first
		writtenAt <- time.Now()
		pw.Write([]byte("late"))
	}()

	buf := make([]byte, len("late"))
	_, err := io.ReadFull(dr, buf)
	elapsed := time.Since(<-writtenAt)

	require.NoError(t, err)
	assert.Equal(t, "late", string(buf))
	assert.GreaterOrEqual(t, elapsed, 90*time.Millisecond, "a chunk read after the ramp period should be held for the full configured delay")
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
