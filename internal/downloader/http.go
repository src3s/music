package downloader

import (
	"net/http"
	"time"
)

// NewOptimizedHTTPClient creates an HTTP client with optimized transport settings
func NewOptimizedHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxConnsPerHost:     0, // unlimited — needed for high parallel chunk counts
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 50,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 5 * time.Second,
			DisableCompression:  false,
			WriteBufferSize:     256 * 1024, // 256KB write buffer
		},
		Timeout: 30 * time.Minute,
	}
}

// DetermineThreads calculates optimal thread count based on file size and user preference
func DetermineThreads(fileSize int64, userThreads int) int {
	if userThreads > 0 {
		return userThreads // respect --threads flag
	}

	// Adaptive logic based on file size
	switch {
	case fileSize < 5*1024*1024: // < 5MB
		return 2
	case fileSize < 20*1024*1024: // < 20MB
		return 4
	case fileSize < 100*1024*1024: // < 100MB
		return 8
	default: // >= 100MB
		return 12
	}
}
