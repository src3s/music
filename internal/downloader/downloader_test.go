package downloader

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		in  string
		out string
	}{
		{"Hello World", "Hello World"},
		{`foo/bar\baz:qux*quux?"quuux"<quuuux>|`, `foo-bar-baz-quxquuxquuuxquuuux`},
		{"  spaced  ", "spaced"},
		{"normal-file.mp3", "normal-file.mp3"},
		{"", ""},
		{"a\nb", "a\nb"}, // newlines preserved (not in replacer)
	}
	for _, tt := range tests {
		got := sanitizeFilename(tt.in)
		if got != tt.out {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tt.in, got, tt.out)
		}
	}
}

func TestParseSizeStr(t *testing.T) {
	tests := []struct {
		in  string
		out int64
	}{
		{"1.5 MiB", 1572864},
		{"2.0 MiB", 2097152},
		{"500 KiB", 512000},
		{"1.2 GiB", 1288490188},
		{"1048576", 1048576},
		{"1.7 MiB", 1782579},
		{"0", 0},
		{"", 0},
	}
	for _, tt := range tests {
		got := parseSizeStr(tt.in)
		if got != tt.out {
			t.Errorf("parseSizeStr(%q) = %d, want %d", tt.in, got, tt.out)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		secs float64
		want string
	}{
		{0, "0:00"},
		{30, "0:30"},
		{60, "1:00"},
		{90, "1:30"},
		{3600, "60:00"},
		{109, "1:49"},
		{3661, "61:01"},
	}
	for _, tt := range tests {
		got := formatDuration(tt.secs)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.secs, got, tt.want)
		}
	}
}

func TestParseYtDlpProgress(t *testing.T) {
	tests := []struct {
		line            string
		wantPct         float64
		wantTotal       int64
		wantCurrent     int64
		wantOk          bool
	}{
		{"[download]  45.2% of 3.24MiB at 1.2 MiB/s ETA 00:02", 45.2, 3397386, 1535618, true},
		{"[download] 100.0% of 1.70MiB at 2.5 MiB/s ETA 00:00", 100.0, 1782579, 1782579, true},
		{"not a download line", 0, 0, 0, false},
		{"[download]  0.0% of unknown size", 0, 0, 0, false},
		{"[download]  45.2% of ~ 3.24MiB at 1.2 MiB/s ETA 00:02", 45.2, 3397386, 1535618, true},
		{"[download]  45.2% of ~3.24MiB at 1.2 MiB/s ETA 00:02", 45.2, 3397386, 1535618, true},
	}
	for _, tt := range tests {
		pct, total, current, ok := parseYtDlpProgress(tt.line)
		if ok != tt.wantOk {
			t.Errorf("parseYtDlpProgress(%q) ok=%v, want %v", tt.line, ok, tt.wantOk)
		}
		if ok {
			if math.Abs(pct-tt.wantPct) > 0.01 {
				t.Errorf("parseYtDlpProgress(%q) pct=%v, want %v", tt.line, pct, tt.wantPct)
			}
			if total != tt.wantTotal {
				t.Errorf("parseYtDlpProgress(%q) total=%d, want %d", tt.line, total, tt.wantTotal)
			}
			if current != tt.wantCurrent {
				t.Errorf("parseYtDlpProgress(%q) current=%d, want %d", tt.line, current, tt.wantCurrent)
			}
		}
	}
}

func TestDetermineThreads(t *testing.T) {
	tests := []struct {
		fileSize int64
		user     int
		want     int
	}{
		{0, 0, 2},
		{1 * 1024 * 1024, 0, 2},
		{10 * 1024 * 1024, 0, 4},
		{50 * 1024 * 1024, 0, 8},
		{200 * 1024 * 1024, 0, 12},
		{0, 16, 16},
		{0, 1, 1},
	}
	for _, tt := range tests {
		got := DetermineThreads(tt.fileSize, tt.user)
		if got != tt.want {
			t.Errorf("DetermineThreads(%d, %d) = %d, want %d", tt.fileSize, tt.user, got, tt.want)
		}
	}
}

func TestChunkWriter(t *testing.T) {
	f, err := os.CreateTemp("", "chunkwriter-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	defer f.Close()

	if err := f.Truncate(100); err != nil {
		t.Fatal(err)
	}

	cw := &chunkWriter{f: f, offset: 10}
	n, err := cw.Write([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("wrote %d bytes, want 5", n)
	}

	buf := make([]byte, 20)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if string(buf[10:15]) != "hello" {
		t.Fatalf("got %q at offset 10, want %q", string(buf[10:15]), "hello")
	}
}

func TestParallelDownloadWithLocalServer(t *testing.T) {
	// Test data: 100KB of repeating pattern
	const dataSize = 100 * 1024
	data := make([]byte, dataSize)
	for i := range data {
		data[i] = byte(i % 256)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", dataSize))
			w.WriteHeader(http.StatusOK)
			w.Write(data)
			return
		}

		var start, end int
		if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if end >= dataSize {
			end = dataSize - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, dataSize))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[start : end+1])
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "output.bin")
	if err := parallelDownload(srv.URL, dest, int64(dataSize), 4, true, nil); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != dataSize {
		t.Fatalf("downloaded %d bytes, want %d", len(got), dataSize)
	}
	for i := range got {
		if got[i] != byte(i%256) {
			t.Fatalf("byte %d: got %02x, want %02x", i, got[i], byte(i%256))
		}
	}
}

func TestRetryOnBadStatus(t *testing.T) {
	data := []byte("retry test data")
	dataSize := int64(len(data))
	var firstAttempt atomic.Bool
	firstAttempt.Store(true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			return
		}
		if firstAttempt.Load() {
			firstAttempt.Store(false)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var start, end int
		fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, dataSize))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[start : end+1])
	}))
	defer srv.Close()

	f, err := os.CreateTemp("", "retry-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	f.Truncate(dataSize)

	// Test that fetchWithRetry handles the 500 and retries
	if err := fetchWithRetry(srv.URL, f, 0, dataSize-1, nil, nil); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, dataSize)
	f.ReadAt(buf, 0)
	if string(buf) != string(data) {
		t.Fatalf("got %q, want %q", string(buf), string(data))
	}
}

func TestProbeURL(t *testing.T) {
	data := []byte("probe test data")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	size, supportsRange, err := probeURL(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(data)) {
		t.Errorf("size = %d, want %d", size, len(data))
	}
	if !supportsRange {
		t.Error("supportsRange = false, want true")
	}
}

func TestParallelDownloadSingleThread(t *testing.T) {
	data := []byte("single thread test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "single.bin")
	if err := parallelDownload(srv.URL, dest, int64(len(data)), 1, true, nil); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("got %q, want %q", string(got), string(data))
	}
}

func TestParallelDownloadZeroThreads(t *testing.T) {
	// Should not panic or divide by zero
	data := []byte("zero threads guard")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "zero.bin")
	if err := parallelDownload(srv.URL, dest, int64(len(data)), 0, true, nil); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("got %q, want %q", string(got), string(data))
	}
}

func TestStreamDownload(t *testing.T) {
	data := []byte("stream download test data")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "stream.bin")
	if err := streamDownload(srv.URL, dest, int64(len(data)), true, nil); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("got %q, want %q", string(got), string(data))
	}
}

func TestTrackProgress(t *testing.T) {
	tp := &TrackProgress{}
	tp.Reset("test title", 1000)

	if tp.Title != "test title" {
		t.Errorf("Title = %q, want %q", tp.Title, "test title")
	}
	if tp.Total.Load() != 1000 {
		t.Errorf("Total = %d, want 1000", tp.Total.Load())
	}

	tp.Current.Add(500)
	if tp.Current.Load() != 500 {
		t.Errorf("Current = %d, want 500", tp.Current.Load())
	}

	tp.Done.Store(true)
	if !tp.Done.Load() {
		t.Error("Done should be true")
	}
}

func TestTpWriter(t *testing.T) {
	tp := &TrackProgress{}
	tp.Reset("test", 100)
	tw := &tpWriter{tp: tp}

	n, err := tw.Write([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("wrote %d, want 5", n)
	}
	if tp.Current.Load() != 5 {
		t.Errorf("Current = %d, want 5", tp.Current.Load())
	}
}

func TestFetchRangeToDisk(t *testing.T) {
	data := make([]byte, 256)
	for i := range data {
		data[i] = byte(i)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
			w.WriteHeader(http.StatusOK)
			w.Write(data)
			return
		}

		var start, end int
		fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[start : end+1])
	}))
	defer srv.Close()

	f, err := os.CreateTemp("", "range-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	defer f.Close()

	f.Truncate(int64(len(data)))

	n, err := fetchRangeToDisk(srv.URL, f, 10, 20, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 11 {
		t.Fatalf("wrote %d bytes, want 11", n)
	}

	buf := make([]byte, 30)
	f.ReadAt(buf, 0)
	for i := 10; i <= 20; i++ {
		if buf[i] != byte(i) {
			t.Fatalf("offset %d: got %02x, want %02x", i, buf[i], byte(i))
		}
	}
}

func TestFetchWithRetry(t *testing.T) {
	data := []byte("retry test")
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if attempts.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
			w.WriteHeader(http.StatusOK)
			w.Write(data)
			return
		}
		var start, end int
		fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[start : end+1])
	}))
	defer srv.Close()

	f, err := os.CreateTemp("", "fetchretry-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	defer f.Close()

	f.Truncate(int64(len(data)))

	if err := fetchWithRetry(srv.URL, f, 0, int64(len(data)-1), nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCleanURL(t *testing.T) {
	tests := []struct {
		in  string
		out string
	}{
		{"https://youtube.com/watch?v=abc&si=xyz", "https://youtube.com/watch?v=abc"},
		{"https://youtube.com/watch?v=abc?si=xyz", "https://youtube.com/watch?v=abc"},
		{"https://youtube.com/watch?v=abc", "https://youtube.com/watch?v=abc"},
		{"https://youtube.com/watch?v=abc&si=xyz&other=val", "https://youtube.com/watch?v=abc"},
	}
	for _, tt := range tests {
		got := cleanURL(tt.in)
		if got != tt.out {
			t.Errorf("cleanURL(%q) = %q, want %q", tt.in, got, tt.out)
		}
	}
}

func TestSanitizeURL(t *testing.T) {
	tests := []struct {
		in  string
		out string
	}{
		{"https://youtube.com/watch?v=abc#fragment", "https://youtube.com/watch?v=abc"},
		{"  https://example.com/video  ", "https://example.com/video"},
		{"not a url", ""},
		{"http://example.com", "http://example.com"},
		{"ftp://bad.com", ""},
	}
	for _, tt := range tests {
		got := sanitizeURL(tt.in)
		if got != tt.out {
			t.Errorf("sanitizeURL(%q) = %q, want %q", tt.in, got, tt.out)
		}
	}
}

func TestNewLineScanner(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte("line1\nline2\nline3\n"))
		pw.Close()
	}()

	s := newLineScanner(pr)
	var lines []string
	for s.Scan() {
		lines = append(lines, s.Text())
	}

	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	if lines[0] != "line1" || lines[1] != "line2" || lines[2] != "line3" {
		t.Fatalf("got %v, want [line1 line2 line3]", lines)
	}
}

func TestIndexByte(t *testing.T) {
	if idx := indexByte([]byte("hello"), 'e'); idx != 1 {
		t.Errorf("indexByte('hello', 'e') = %d, want 1", idx)
	}
	if idx := indexByte([]byte("hello"), 'z'); idx != -1 {
		t.Errorf("indexByte('hello', 'z') = %d, want -1", idx)
	}
	if idx := indexByte([]byte{}, 'a'); idx != -1 {
		t.Errorf("indexByte(empty, 'a') = %d, want -1", idx)
	}
}

func TestPool256K(t *testing.T) {
	buf := pool256k.Get().([]byte)
	if len(buf) != 256*1024 {
		t.Fatalf("buffer len = %d, want %d", len(buf), 256*1024)
	}
	pool256k.Put(buf)
}

func TestParallelDownloadConcurrentWrites(t *testing.T) {
	// Verify that parallel chunk writes don't race
	const dataSize = 50 * 1024
	data := make([]byte, dataSize)
	for i := range data {
		data[i] = byte(i % 251)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", dataSize))
			w.WriteHeader(http.StatusOK)
			w.Write(data)
			return
		}
		var start, end int
		fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)
		if end >= dataSize {
			end = dataSize - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, dataSize))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[start : end+1])
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "concurrent.bin")

	// Run multiple times to catch races
	for i := 0; i < 5; i++ {
		if err := parallelDownload(srv.URL, dest, int64(dataSize), 16, true, nil); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(dest)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != dataSize {
			t.Fatalf("run %d: size = %d, want %d", i, len(got), dataSize)
		}
		for j := range got {
			if got[j] != byte(j%251) {
				t.Fatalf("run %d byte %d: got %02x, want %02x", i, j, got[j], byte(j%251))
			}
		}
	}
}

func TestDisplayArtist(t *testing.T) {
	v := &VideoInfo{Artist: "Artist", Uploader: "Uploader"}
	if v.DisplayArtist() != "Artist" {
		t.Errorf("DisplayArtist with Artist set = %q, want Artist", v.DisplayArtist())
	}

	v2 := &VideoInfo{Artist: "", Uploader: "Uploader"}
	if v2.DisplayArtist() != "Uploader" {
		t.Errorf("DisplayArtist without Artist = %q, want Uploader", v2.DisplayArtist())
	}

	v3 := &VideoInfo{Artist: "", Uploader: ""}
	if v3.DisplayArtist() != "" {
		t.Errorf("DisplayArtist empty = %q, want empty", v3.DisplayArtist())
	}
}

func TestFormatDurationEdge(t *testing.T) {
	// Edge cases
	if got := formatDuration(math.MaxFloat64); got != "" {
		// Just make sure it doesn't panic
	}
}

func TestMultipleGoroutinePool(t *testing.T) {
	// Verify pool256k is safe under concurrent access
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := pool256k.Get().([]byte)
			if len(buf) != 256*1024 {
				panic("bad buffer size")
			}
			for j := range buf {
				buf[j] = byte(j)
			}
			pool256k.Put(buf)
		}()
	}
	wg.Wait()
}

func TestChunkWriterMultipleChunks(t *testing.T) {
	f, err := os.CreateTemp("", "chunkmulti-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	defer f.Close()

	if err := f.Truncate(100); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		cw := &chunkWriter{f: f, offset: 0}
		cw.Write([]byte("AAAAA"))
	}()

	go func() {
		defer wg.Done()
		cw := &chunkWriter{f: f, offset: 50}
		cw.Write([]byte("BBBBB"))
	}()

	wg.Wait()

	buf := make([]byte, 100)
	f.ReadAt(buf, 0)

	if string(buf[0:5]) != "AAAAA" {
		t.Fatalf("chunk 0: got %q", string(buf[0:5]))
	}
	if string(buf[50:55]) != "BBBBB" {
		t.Fatalf("chunk 50: got %q", string(buf[50:55]))
	}
}
