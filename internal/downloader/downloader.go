package downloader

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fatih/color"
	"github.com/ohcass/music/internal/metadata"
	"github.com/ohcass/music/internal/progress"
)

// TrackProgress lets playlist mode track per-slot byte progress in real time.
type TrackProgress struct {
	Title      string
	Current    atomic.Int64
	Total      atomic.Int64
	Done       atomic.Bool
	Failed     atomic.Bool
	Converting atomic.Bool
	ConvertPct atomic.Int64 // 0-100
}

func (t *TrackProgress) Reset(title string, total int64) {
	t.Title = title
	t.Current.Store(0)
	t.Total.Store(total)
	t.Done.Store(false)
	t.Failed.Store(false)
	t.Converting.Store(false)
	t.ConvertPct.Store(0)
}

type VideoInfo struct {
	Title          string  `json:"title"`
	Artist         string  `json:"artist"`
	Uploader       string  `json:"uploader"`
	Album          string  `json:"album"`
	Duration       float64 `json:"duration"`
	Height         int     `json:"height"`
	URL            string  `json:"url"`
	Thumbnail      string  `json:"thumbnail"`
	Ext            string  `json:"ext"`
	AudioCodec     string  `json:"acodec"`
	Filesize       int64   `json:"filesize"`
	FilesizeApprox int64   `json:"filesize_approx"`
}

func (v *VideoInfo) DisplayArtist() string {
	if v.Artist != "" {
		return v.Artist
	}
	return v.Uploader
}

var httpClient = NewOptimizedHTTPClient()

// pool256k provides reusable 256KB buffers for streaming downloads.
var pool256k = sync.Pool{New: func() any { b := make([]byte, 256*1024); return b }}

const maxRetries = 10

// buildCommand creates an exec.Cmd with the given name and arguments
func buildCommand(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}

// findBin finds the path to a binary, can be overridden for testing
var findBin = func(name string) string {
	if runtime.GOOS == "windows" && !strings.HasSuffix(name, ".exe") {
		name += ".exe"
	}
	if dir := selfDir(); dir != "" {
		if candidate := filepath.Join(dir, name); fileExists(candidate) {
			return candidate
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if candidate := filepath.Join(home, ".radii5", "bin", name); fileExists(candidate) {
			return candidate
		}
	}
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	return name
}

func selfDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func Download(url, format, outputDir string, threads int, silent bool, tp *TrackProgress, mediaType string, quality int) error {
	bold := color.New(color.FgWhite, color.Bold)
	cyan := color.New(color.FgCyan)

	if !silent {
		fmt.Println()
		cyan.Print("  → ")
		bold.Println("Resolving track...")
	}

	info, err := resolve(url, mediaType, quality)
	if err != nil {
		if tp != nil {
			tp.Failed.Store(true)
		}
		return fmt.Errorf("could not resolve URL: %w", err)
	}

	if tp != nil {
		size := info.Filesize
		if size == 0 {
			size = info.FilesizeApprox
		}
		tp.Reset(info.Title, size)
	}

	if !silent {
		fmt.Println()
		color.New(color.FgHiWhite, color.Bold).Printf("  %s\n", info.Title)
		if artist := info.DisplayArtist(); artist != "" {
			color.New(color.FgHiBlack).Printf("  %s\n", artist)
		}
		if info.Duration > 0 {
			color.New(color.FgHiBlack).Printf("  %s\n", formatDuration(info.Duration))
		}
		fmt.Println()
	}

	safeTitle := sanitizeFilename(info.Title)

	outExt := format
	if mediaType == "video" {
		outExt = "mp4"
		outputDir = filepath.Join(outputDir, "video")
	}
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("cannot create output dir: %w", err)
	}
	outFile := filepath.Join(outputDir, safeTitle+"."+outExt)

	// For video, always go through yt-dlp (handles stream merging, no audio conversion)
	if mediaType == "video" {
		if err := ytDlpFallback(url, format, outFile, threads, silent, mediaType, quality, tp); err != nil {
			if tp != nil {
				tp.Failed.Store(true)
			}
			return err
		}
		if tp != nil {
			tp.Done.Store(true)
		}
		return nil
	}

	tmpFile := filepath.Join(outputDir, safeTitle+".tmp")

	// Ensure temp file cleanup on panic or error
	defer func() {
		if r := recover(); r != nil {
			os.Remove(tmpFile)
			panic(r) // Re-panic after cleanup
		}
		if err != nil {
			os.Remove(tmpFile)
		}
	}()

	// If direct URL available, download ourselves (fast parallel chunks).
	// Otherwise fall back to yt-dlp for extraction + conversion.
	if info.URL != "" {
		size := info.Filesize
		if size == 0 {
			size = info.FilesizeApprox
		}

		start := time.Now()

		// Always download to temp file (parallel if Range+threads>1), then convert/rename
		_, supportsRange, _ := probeURL(info.URL)
		tmpFile := outFile + ".tmp"

		if supportsRange && size > 0 && threads > 1 {
			if !silent {
				cyan.Printf("  → Downloading in %d parallel chunks...\n\n", threads)
			}
			if err := parallelDownload(info.URL, tmpFile, size, threads, silent, tp); err != nil {
				os.Remove(tmpFile)
				if tp != nil {
					tp.Failed.Store(true)
				}
				return fmt.Errorf("download failed: %w", err)
			}
		} else {
			if !silent {
				cyan.Println("  → Downloading...")
				fmt.Println()
			}
			if err := streamDownload(info.URL, tmpFile, size, silent, tp); err != nil {
				os.Remove(tmpFile)
				if tp != nil {
					tp.Failed.Store(true)
				}
				return fmt.Errorf("download failed: %w", err)
			}
		}

	if format != info.Ext {
		if tp != nil {
			tp.Converting.Store(true)
			tp.ConvertPct.Store(3)
		}
		if err := convertAudio(tmpFile, outFile, format); err != nil {
			os.Remove(tmpFile)
			if tp != nil {
				tp.Converting.Store(false)
				tp.Failed.Store(true)
			}
			return fmt.Errorf("conversion failed: %w", err)
		}
		if tp != nil {
			tp.Converting.Store(false)
		}
		os.Remove(tmpFile)
		elapsed := time.Since(start)
		if !silent {
			color.New(color.FgGreen, color.Bold).Printf("\n  ✓ Saved to %s", color.New(color.FgCyan).Sprint(outFile))
			color.New(color.FgHiBlack).Printf("  (%s)\n\n", elapsed.Round(time.Millisecond))
		}
	} else {
		if err := os.Rename(tmpFile, outFile); err != nil {
			os.Remove(tmpFile)
			if tp != nil {
				tp.Failed.Store(true)
			}
			return fmt.Errorf("rename failed: %w", err)
		}
		elapsed := time.Since(start)
		if !silent {
			if fi, err := os.Stat(outFile); err == nil {
				if sz := fi.Size(); sz > 0 && elapsed.Seconds() > 0 {
					mbps := float64(sz) / (1 << 20) / elapsed.Seconds()
					if supportsRange && size > 0 && threads > 1 {
						color.New(color.FgHiBlack).Printf("  %.1f MB/s  (%.1fs,  %d threads)\n", mbps, elapsed.Seconds(), threads)
					} else {
						color.New(color.FgHiBlack).Printf("  %.1f MB/s  (%.1fs)\n", mbps, elapsed.Seconds())
					}
				}
			}
			color.New(color.FgGreen, color.Bold).Printf("  ✓ Saved to %s", color.New(color.FgCyan).Sprint(outFile))
			color.New(color.FgHiBlack).Printf("  (%s)\n\n", elapsed.Round(time.Millisecond))
		}
	}

	if format == "mp3" && mediaType != "video" {
		if !silent {
			cyan.Print("  → Writing metadata...")
		}
		_ = metadata.WriteMP3Tags(outFile, info.Title, info.DisplayArtist(), info.Album, info.Thumbnail)
		if !silent {
			cyan.Println(" done")
		}
	}

	if tp != nil {
		tp.Done.Store(true)
	}

	} else {
		// No direct URL — let yt-dlp handle the full download + conversion
		if err := ytDlpFallback(url, format, outFile, threads, silent, mediaType, quality, tp); err != nil {
			if tp != nil {
				tp.Failed.Store(true)
			}
			return err
		}
		if tp != nil {
			tp.Done.Store(true)
		}
	}

	return nil
}

func sanitizeURL(inputURL string) string {
	inputURL = strings.TrimSpace(inputURL)
	if !strings.HasPrefix(inputURL, "http://") && !strings.HasPrefix(inputURL, "https://") {
		return ""
	}

	parsed, err := url.Parse(inputURL)
	if err != nil {
		return ""
	}

	if parsed.Host == "" {
		return ""
	}

	parsed.Fragment = ""
	return parsed.String()
}

func resolve(url string, mediaType string, quality int) (*VideoInfo, error) {
	url = sanitizeURL(url)
	if url == "" {
		return nil, fmt.Errorf("invalid URL")
	}

	// Check for playlist URLs and reject them early
	if strings.Contains(url, "list=") || strings.Contains(url, "/playlist") {
		return nil, fmt.Errorf("playlist URLs not supported - use individual video URLs")
	}

	url = cleanURL(url)
	ytdlp := findBin("yt-dlp")

	// Create context with timeout for yt-dlp resolve operation
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	formatFlag := "bestaudio"
	if mediaType == "video" {
		if quality > 0 {
			formatFlag = fmt.Sprintf("bestvideo[height<=?%d]+bestaudio/best[height<=?%d]", quality, quality)
		} else {
			formatFlag = "bestvideo+bestaudio/best"
		}
	}

	cmd := exec.CommandContext(ctx, ytdlp,
		"--dump-json",
		"--format", formatFlag,
		"--no-playlist",
		"--socket-timeout", "25", // yt-dlp socket timeout (25s < 30s context)
		url,
	)

	// Capture both stdout and stderr
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	err := cmd.Start()
	if err != nil {
		return nil, fmt.Errorf("failed to start yt-dlp: %w", err)
	}

	err = cmd.Wait()

	// Check for context errors first
	if ctx.Err() == context.Canceled {
		return nil, fmt.Errorf("yt-dlp resolve canceled: %w", ctx.Err())
	}
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("yt-dlp resolve timeout after 30 seconds")
	}

	if err != nil {
		// Check if yt-dlp binary exists
		if _, e := exec.LookPath(ytdlp); e != nil {
			return nil, fmt.Errorf("yt-dlp not found — run the installer")
		}

		// Return structured error with stderr
		stderr := stderrBuf.String()
		if stderr != "" {
			return nil, fmt.Errorf("yt-dlp resolve failed: %w\nstderr: %s", err, stderr)
		}
		return nil, fmt.Errorf("yt-dlp resolve failed: %w", err)
	}

	var info VideoInfo
	if err := json.Unmarshal(stdoutBuf.Bytes(), &info); err != nil {
		return nil, fmt.Errorf("failed to parse track info: %w", err)
	}
	return &info, nil
}

func ytDlpFallback(url, format, outFile string, threads int, silent bool, mediaType string, quality int, tp *TrackProgress) error {
	cyan := color.New(color.FgCyan)
	adaptiveThreads := DetermineThreads(0, threads) // 0 size = unknown, use adaptive logic
	if !silent {
		cyan.Printf("  → Downloading via yt-dlp (%d fragments)...\n\n", adaptiveThreads)
	}

	// Create context with timeout for yt-dlp operation
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	ytdlp := findBin("yt-dlp")
	var args []string
	if mediaType == "audio" {
		args = []string{
			"--no-playlist",
			"-x",
			"--audio-format", format,
			"--audio-quality", "2",
			"--concurrent-fragments", fmt.Sprintf("%d", adaptiveThreads),
			"--no-colors",
			"--progress", "--newline",
			"-o", outFile,
			url,
		}
	} else {
		formatStr := "bestvideo+bestaudio/best"
		if quality > 0 {
			formatStr = fmt.Sprintf("bestvideo[height<=?%d]+bestaudio/best[height<=?%d]", quality, quality)
		}
		args = []string{
			"--no-playlist",
			"--format", formatStr,
			"--merge-output-format", "mp4",
			"--concurrent-fragments", fmt.Sprintf("%d", adaptiveThreads),
			"--no-colors",
			"--progress", "--newline",
			"-o", outFile,
			url,
		}
	}

	cmd := exec.CommandContext(ctx, ytdlp, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		if _, e := exec.LookPath(ytdlp); e != nil {
			return fmt.Errorf("yt-dlp not found — run the installer: %w", e)
		}
		return fmt.Errorf("yt-dlp failed to start: %w", err)
	}

	var bar *progress.Bar
	var mu sync.Mutex
	var once sync.Once

	scan := func(r io.Reader, isErr bool) {
		scanner := newLineScanner(r)
		for scanner.Scan() {
			line := scanner.Text()
			pct, dlTotal, dlCurrent, ok := parseYtDlpProgress(line)
			if ok {
				if tp != nil {
					if dlCurrent <= 0 && dlTotal > 0 {
						dlCurrent = 1
					}
					tp.Total.Store(dlTotal)
					tp.Current.Store(dlCurrent)
				}
				if !silent {
					mu.Lock()
					if bar == nil {
						bar = progress.NewBar(dlTotal)
					}
					bar.Set(dlCurrent)
					if pct >= 100 {
						once.Do(func() { bar.Finish() })
					}
					mu.Unlock()
				}
			} else if isErr && strings.Contains(line, "ERROR") && !silent {
				fmt.Fprintf(os.Stderr, "  %s\n", color.RedString(line))
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); scan(stdout, false) }()
	go func() { defer wg.Done(); scan(stderr, true) }()

	err = cmd.Wait()
	wg.Wait()

	mu.Lock()
	if bar != nil {
		once.Do(func() { bar.Finish() })
	}
	mu.Unlock()

	if err != nil {
		if ctx.Err() == context.Canceled {
			return fmt.Errorf("yt-dlp canceled: %w", ctx.Err())
		}
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("yt-dlp timeout after 30 minutes: %w", err)
		}
		return fmt.Errorf("yt-dlp failed: %w", err)
	}

	if !silent {
		color.New(color.FgGreen, color.Bold).Printf("  ✓ Saved to %s\n\n", outFile)
	}
	return nil
}

func probeURL(url string) (int64, bool, error) {
	resp, err := httpClient.Head(url)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	size := resp.ContentLength
	supportsRange := resp.Header.Get("Accept-Ranges") == "bytes"
	return size, supportsRange, nil
}

// chunkWriter adapts *os.File for concurrent WriteAt from multiple goroutines.
// Each chunk gets its own writer starting at a fixed file offset.
type chunkWriter struct {
	f      *os.File
	offset int64
	pos    int64
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	n, err := w.f.WriteAt(p, w.offset+w.pos)
	w.pos += int64(n)
	return n, err
}

func parallelDownload(url, dest string, size int64, threads int, silent bool, tp *TrackProgress) error {
	if threads < 1 {
		threads = 1
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return err
	}

	chunkSize := size / int64(threads)
	var wg sync.WaitGroup
	errCh := make(chan error, threads)
	var bar *progress.Bar
	if !silent {
		bar = progress.NewBar(size)
	}

	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start := int64(i) * chunkSize
			end := start + chunkSize - 1
			if i == threads-1 {
				end = size - 1
			}
			if err := fetchWithRetry(url, f, start, end, bar, tp); err != nil {
				errCh <- fmt.Errorf("chunk %d: %w", i, err)
			}
		}(i)
	}

	wg.Wait()
	f.Close()
	if bar != nil {
		bar.Finish()
	}
	close(errCh)

	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

func fetchWithRetry(url string, f *os.File, start, end int64, bar *progress.Bar, tp *TrackProgress) error {
	current := start
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			delay := 1 << uint(min(attempt-1, 5))
			time.Sleep(time.Duration(delay) * time.Second)
		}
		written, err := fetchRangeToDisk(url, f, current, end, bar, tp)
		current += written
		if err == nil {
			return nil
		}
		if current > end {
			return nil
		}
	}
	return fmt.Errorf("failed after %d retries", maxRetries)
}

func fetchRangeToDisk(url string, f *os.File, start, end int64, bar *progress.Bar, tp *TrackProgress) (int64, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; radii5/0.1)")

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("unexpected HTTP %d", resp.StatusCode)
	}

	cw := &chunkWriter{f: f, offset: start}
	var writers []io.Writer
	writers = append(writers, cw)
	if bar != nil {
		writers = append(writers, bar)
	}
	if tp != nil {
		writers = append(writers, &tpWriter{tp: tp})
	}

	buf := pool256k.Get().([]byte)
	defer pool256k.Put(buf)
	n, err := io.CopyBuffer(io.MultiWriter(writers...), resp.Body, buf)
	return n, err
}

type tpWriter struct {
	tp *TrackProgress
}

func (w *tpWriter) Write(p []byte) (int, error) {
	w.tp.Current.Add(int64(len(p)))
	return len(p), nil
}

func streamDownload(url, dest string, size int64, silent bool, tp *TrackProgress) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; radii5/0.1)")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	var writers []io.Writer
	writers = append(writers, f)
	var bar *progress.Bar
	if !silent {
		bar = progress.NewBar(size)
		writers = append(writers, bar)
	}
	if tp != nil {
		writers = append(writers, &tpWriter{tp: tp})
	}

	buf := pool256k.Get().([]byte)
	defer pool256k.Put(buf)
	_, err = io.CopyBuffer(io.MultiWriter(writers...), resp.Body, buf)
	if bar != nil {
		bar.Finish()
	}
	return err
}

func convertAudio(input, output, format string) error {
	ffmpeg := findBin("ffmpeg")
	var args []string
	switch format {
	case "mp3":
		args = []string{"-i", input, "-codec:a", "libmp3lame", "-qscale:a", "2", "-y", output}
	case "flac":
		args = []string{"-i", input, "-codec:a", "flac", "-compression_level", "5", "-y", output}
	case "m4a":
		args = []string{"-i", input, "-codec:a", "aac", "-b:a", "256k", "-y", output}
	default:
		args = []string{"-i", input, "-y", output}
	}
	cmd := exec.Command(ffmpeg, args...)
	cmd.Stderr = io.Discard
	return cmd.Run()
}

// convertAudioProgress runs ffmpeg with -progress pipe:1 so we can
// parse out_time_ms and report conversion percentage via tp.
func convertAudioProgress(input, output, format string, durationSecs float64, tp *TrackProgress) error {
	ffmpeg := findBin("ffmpeg")
	var args []string
	switch format {
	case "mp3":
		args = []string{"-i", input, "-codec:a", "libmp3lame", "-qscale:a", "2", "-progress", "pipe:1", "-y", output}
	case "flac":
		args = []string{"-i", input, "-codec:a", "flac", "-compression_level", "5", "-progress", "pipe:1", "-y", output}
	case "m4a":
		args = []string{"-i", input, "-codec:a", "aac", "-b:a", "256k", "-progress", "pipe:1", "-y", output}
	default:
		args = []string{"-i", input, "-progress", "pipe:1", "-y", output}
	}

	cmd := exec.Command(ffmpeg, args...)
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return convertAudio(input, output, format)
	}
	if err := cmd.Start(); err != nil {
		return convertAudio(input, output, format)
	}

	durationMs := int64(durationSecs * 1000000)

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "out_time_ms=") {
			val := strings.TrimPrefix(line, "out_time_ms=")
			if ms, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil && durationMs > 0 {
				pct := int64(float64(ms) / float64(durationMs) * 100)
				if pct > 100 {
					pct = 100
				}
				if pct < 0 {
					pct = 0
				}
				tp.ConvertPct.Store(pct)
			}
		}
	}

	return cmd.Wait()
}

func cleanURL(raw string) string {
	for _, param := range []string{"?si=", "&si="} {
		if idx := strings.Index(raw, param); idx != -1 {
			raw = raw[:idx]
		}
	}
	return raw
}

func sanitizeFilename(name string) string {
	replacer := strings.NewReplacer(
		"/", "-", "\\", "-", ":", "-", "*", "",
		"?", "", "\"", "", "<", "", ">", "", "|", "",
	)
	return strings.TrimSpace(replacer.Replace(name))
}

func formatDuration(secs float64) string {
	m := int(secs) / 60
	s := int(secs) % 60
	return fmt.Sprintf("%d:%02d", m, s)
}

func newLineScanner(r io.Reader) *lineScanner {
	return &lineScanner{r: r, buf: make([]byte, 0, 4096)}
}

type lineScanner struct {
	r    io.Reader
	buf  []byte
	line string
	done bool
}

func (s *lineScanner) Scan() bool {
	if s.done {
		return false
	}
	tmp := make([]byte, 512)
	for {
		if idx := indexByte(s.buf, '\n'); idx >= 0 {
			s.line = strings.TrimRight(string(s.buf[:idx]), "\r")
			s.buf = s.buf[idx+1:]
			return true
		}
		n, err := s.r.Read(tmp)
		if n > 0 {
			s.buf = append(s.buf, tmp[:n]...)
		}
		if err != nil {
			if len(s.buf) > 0 {
				s.line = strings.TrimRight(string(s.buf), "\r\n")
				s.buf = nil
				s.done = true
				return s.line != ""
			}
			return false
		}
	}
}

func (s *lineScanner) Text() string { return s.line }

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

func parseYtDlpProgress(line string) (pct float64, total, current int64, ok bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "[download]") {
		return
	}
	line = strings.TrimPrefix(line, "[download]")
	line = strings.TrimSpace(line)

	ofIdx := strings.Index(line, "% of")
	if ofIdx < 0 {
		return
	}

	pctStr := strings.TrimSpace(line[:ofIdx])
	if _, err := fmt.Sscanf(pctStr, "%f", &pct); err != nil {
		return
	}

	rest := strings.TrimSpace(line[ofIdx+4:])
	rest = strings.TrimLeft(rest, "~ ") // yt-dlp prefixes approx size with "~"
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return
	}
	total = parseSizeStr(fields[0])
	if total <= 0 {
		return
	}
	current = int64(float64(total) * pct / 100)
	ok = true
	return
}

func parseSizeStr(s string) int64 {
	s = strings.ReplaceAll(s, ",", "")
	var val float64
	var unit string
	fmt.Sscanf(s, "%f%s", &val, &unit)
	unit = strings.ToLower(unit)
	switch {
	case strings.HasPrefix(unit, "gib") || strings.HasPrefix(unit, "gb"):
		return int64(val * 1073741824)
	case strings.HasPrefix(unit, "mib") || strings.HasPrefix(unit, "mb"):
		return int64(val * 1048576)
	case strings.HasPrefix(unit, "kib") || strings.HasPrefix(unit, "kb"):
		return int64(val * 1024)
	default:
		return int64(val)
	}
}
