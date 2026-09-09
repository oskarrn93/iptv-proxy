package iptvproxy

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"time"
)

// delayChunk is one read from the upstream side of a delayedReader, tagged
// with when it was read so Read can hold it until bufferDuration has
// elapsed. err is set instead of data on the final chunk, carrying the
// upstream's terminal error (io.EOF included) through the same channel
// rather than a second signaling path.
type delayChunk struct {
	data []byte
	err  error
	at   time.Time
}

// delayedReader wraps src so every byte read from it is held for delay
// before Read returns it. Reading continues from src in the background the
// whole time, so once warmed up the reader always has roughly delay's worth
// of data already sitting in chunks — a stall in src of up to delay is
// invisible to whatever is reading from the delayedReader, at the cost of
// adding that same delay before the first byte comes out.
type delayedReader struct {
	ctx    context.Context
	delay  time.Duration
	chunks chan delayChunk
	buf    []byte
	err    error
}

func newDelayedReader(ctx context.Context, src io.Reader, delay time.Duration) *delayedReader {
	dr := &delayedReader{
		ctx: ctx,
		// Buffered generously enough that a fast producer doesn't stall on
		// channel sends between the (rare) Read calls that drain it — actual
		// memory use is bounded by delay * upstream bitrate regardless, since
		// production can only run delay ahead of consumption before Read
		// starts blocking on the elapsed-time check anyway.
		chunks: make(chan delayChunk, 256),
		delay:  delay,
	}
	go dr.pump(src)
	return dr
}

// pump reads src to completion (or until ctx is done) and forwards every
// chunk, timestamped, to dr.chunks — including the terminal error, so Read
// never needs a second channel to learn the stream ended.
func (dr *delayedReader) pump(src io.Reader) {
	defer close(dr.chunks)
	buf := make([]byte, bufferSize)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			select {
			case dr.chunks <- delayChunk{data: chunk, at: time.Now()}:
			case <-dr.ctx.Done():
				return
			}
		}
		if err != nil {
			select {
			case dr.chunks <- delayChunk{err: err, at: time.Now()}:
			case <-dr.ctx.Done():
			}
			return
		}
	}
}

// Read implements io.Reader, releasing each chunk pump read only once delay
// has passed since it read it.
func (dr *delayedReader) Read(p []byte) (int, error) {
	if len(dr.buf) == 0 {
		if dr.err != nil {
			return 0, dr.err
		}
		var chunk delayChunk
		var ok bool
		select {
		case chunk, ok = <-dr.chunks:
		case <-dr.ctx.Done():
			return 0, dr.ctx.Err()
		}
		if !ok {
			return 0, io.EOF
		}
		if wait := time.Until(chunk.at.Add(dr.delay)); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-dr.ctx.Done():
				timer.Stop()
				return 0, dr.ctx.Err()
			}
		}
		if chunk.err != nil {
			dr.err = chunk.err
			if len(chunk.data) == 0 {
				return 0, dr.err
			}
		}
		dr.buf = chunk.data
	}
	n := copy(p, dr.buf)
	dr.buf = dr.buf[n:]
	return n, nil
}

// pipeResponseWriter adapts an io.Writer to http.ResponseWriter so
// streamWithReconnect — which needs a ResponseWriter to write and flush a
// live stream's bytes — can be pointed at an io.Pipe instead of the real
// client connection. Header/WriteHeader are meaningless for a pipe and
// Flush needs no implementation: bufferedCopy already tolerates
// http.ErrNotSupported from http.NewResponseController when the underlying
// writer doesn't support flushing, and an io.PipeWriter's Write already
// blocks until its reader consumes the bytes, so there's nothing to flush.
type pipeResponseWriter struct {
	w io.Writer
}

func (p pipeResponseWriter) Header() http.Header         { return http.Header{} }
func (p pipeResponseWriter) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p pipeResponseWriter) WriteHeader(int)             {}

// streamLive serves a live TS stream, optionally through the delay buffer
// described on delayedReader. Disabled, it's exactly streamWithReconnect;
// enabled, streamWithReconnect instead writes into an in-memory pipe (so its
// own reconnect/gap-bridging logic is unchanged) and a delayedReader between
// that pipe and the real client absorbs upstream hiccups shorter than the
// configured duration before they ever reach the client.
func (s *feedService) streamLive(ctx context.Context, w http.ResponseWriter, target *url.URL, initialBody io.ReadCloser) error {
	if s.clientBufferDelay == nil {
		return s.streamWithReconnect(ctx, w, target, initialBody)
	}

	// producerCtx is canceled as soon as this function stops reading from the
	// buffer (client disconnected downstream, or the real write to w failed)
	// so streamWithReconnect's own ctx.Err() check ends it promptly instead
	// of retrying against an upstream nothing is consuming anymore.
	producerCtx, cancelProducer := context.WithCancel(ctx)
	defer cancelProducer()

	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		err := s.streamWithReconnect(producerCtx, pipeResponseWriter{pw}, target, initialBody)
		pw.CloseWithError(err)
		errCh <- err
	}()

	dr := newDelayedReader(ctx, pr, *s.clientBufferDelay)
	_, copyErr := bufferedCopy(w, dr, nil)

	// Unblock the producer if it's still writing: canceling producerCtx alone
	// doesn't interrupt an in-flight, already-blocked pipe Write, only the
	// next ctx.Err() check.
	cancelProducer()
	pr.CloseWithError(copyErr)
	producerErr := <-errCh

	if copyErr != nil {
		return copyErr
	}
	return producerErr
}
