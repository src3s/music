package progress

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// Bar is a thread-safe progress bar that tracks downloaded bytes
type Bar struct {
	total    int64
	current  atomic.Int64
	lastPrint int64
	finished atomic.Bool
}

func NewBar(total int64) *Bar {
	return &Bar{total: total}
}

// Write implements io.Writer so it can be used with TeeReader
func (b *Bar) Write(p []byte) (int, error) {
	n := len(p)
	if !b.finished.Load() {
		b.current.Add(int64(n))
		b.render()
	}
	return n, nil
}

// Set forces the bar to a specific byte position (used when parsing external progress).
func (b *Bar) Set(current int64) {
	if !b.finished.Load() {
		b.current.Store(current)
		b.render()
	}
}

func (b *Bar) render() {
	current := b.current.Load()

	// Throttle — only redraw every 50KB
	if current-b.lastPrint < 51200 && current < b.total {
		return
	}
	b.lastPrint = current

	width := 30
	var filled int
	var pct float64

	if b.total > 0 {
		pct = float64(current) / float64(b.total)
		filled = int(pct * float64(width))
	} else {
		filled = width / 2 // spinner-style if unknown size
	}

	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)

	speed := ""
	if b.total > 0 {
		speed = fmt.Sprintf("  %s / %s", formatBytes(current), formatBytes(b.total))
	} else {
		speed = fmt.Sprintf("  %s", formatBytes(current))
	}

	fmt.Printf("\r  \033[36m[%s]\033[0m%s\033[K", bar, speed)
}

func (b *Bar) Finish() {
	if !b.finished.CompareAndSwap(false, true) {
		return
	}
	if b.total > 0 {
		bar := strings.Repeat("█", 30)
		fmt.Printf("\r  \033[32m[%s]\033[0m  %s ✓%-10s\n", bar, formatBytes(b.total), "")
	} else {
		fmt.Printf("\r%-60s\n", "")
	}
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
