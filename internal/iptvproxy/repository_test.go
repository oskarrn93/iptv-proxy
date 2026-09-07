package iptvproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsTransientError(t *testing.T) {
	assert.True(t, isTransientError(context.DeadlineExceeded))
	assert.True(t, isTransientError(io.EOF))
	assert.True(t, isTransientError(io.ErrUnexpectedEOF))
	assert.False(t, isTransientError(context.Canceled))
	assert.False(t, isTransientError(errors.New("some other error")))
}

func TestIsRetryableStatus(t *testing.T) {
	assert.True(t, isRetryableStatus(http.StatusTooManyRequests))
	assert.True(t, isRetryableStatus(http.StatusInternalServerError))
	assert.True(t, isRetryableStatus(http.StatusBadGateway))
	assert.True(t, isRetryableStatus(http.StatusServiceUnavailable))
	assert.True(t, isRetryableStatus(http.StatusGatewayTimeout))
	assert.False(t, isRetryableStatus(http.StatusOK))
	assert.False(t, isRetryableStatus(http.StatusNotFound))
	assert.False(t, isRetryableStatus(http.StatusUnauthorized))
}
