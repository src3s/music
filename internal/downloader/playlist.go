package downloader

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fatih/color"
	"github.com/ohcass/music/internal/metadata"
)

type PlaylistEntry struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	URL           string `json:"url"`
	WebpageURL    string `json:"webpage_url"`
	PlaylistTitle string `json:"playlist_title"`
}

type resolvedEntry struct {
	entry PlaylistEntry
	info  *VideoInfo
	err   error
}

func ResolvePlaylist(playlistURL string) ([]PlaylistEntry, string, error) {
	ytdlp := findBin("yt-dlp")
	cmd := buildCommand(ytdlp,
		"--flat-playlist",
		"--dump-json",
		"--no-warnings",
		playlistURL,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", err
	}
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("yt-dlp not found")
	}

	var entries []PlaylistEntry
	playlistTitle := ""
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e PlaylistEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		if e.WebpageURL == "" && e.ID != "" {
			e.WebpageURL = "https://youtube.com/watch?v=" + e.ID
		}
		if e.WebpageURL != "" {
			entries = append(entries, e)
		}
		if playlistTitle == "" && e.PlaylistTitle != "" {
			playlistTitle = e.PlaylistTitle
		}
	}

	if playlistTitle == "" {
		playlistTitle = "playlist"
	}

	return entries, playlistTitle, nil
}

const titleWidth = 46

func truncTitle(title string) string {
	runes := []rune(title)
	if len(runes) > titleWidth {
		return string(runes[:titleWidth-3]) + "..."
	}

	s := string(runes)
	for len([]rune(s)) < titleWidth {
		s += " "
	}
	return s
}

type slotState struct {
	tp          *TrackProgress
	oldTitle    string
	oldFailed   bool
	slideOffset int
	sliding     bool
}

func renderTitle(s *slotState) string {
	tp := s.tp
	title := tp.Title
	current := tp.Current.Load()
	total := tp.Total.Load()
	done := tp.Done.Load()
	failed := tp.Failed.Load()
	converting := tp.Converting.Load()
	convertPct := tp.ConvertPct.Load()

	display := truncTitle(title)

	if s.sliding {
		oldStr := truncTitle(s.oldTitle)
		offset := s.slideOffset
		if offset > titleWidth {
			offset = titleWidth
		}

		newRunes := []rune(display)
		oldRunes := []rune(oldStr)

		leftPart := string(newRunes[titleWidth-offset : titleWidth])
		rightPart := string(oldRunes[0 : titleWidth-offset])

		oldColor := "\033[90m"
		if s.oldFailed {
			oldColor = "\033[31m"
		}

		return fmt.Sprintf("  \033[36m→  \033[90m%s\033[0m%s%s\033[0m", leftPart, oldColor, rightPart)
	}

	if failed {
		return fmt.Sprintf("  \033[31m✗  %s\033[0m", display)
	}

	if done {
		return fmt.Sprintf("  \033[36m✓  \033[36m%s\033[0m", display)
	}

	if converting {
		// cyan sweep right→left: filled from right side
		runes := []rune(display)
		filled := int(float64(convertPct) / 100.0 * float64(len(runes)))
		if filled > len(runes) {
			filled = len(runes)
		}
		if filled < 0 {
			filled = 0
		}
		rest := string(runes[:len(runes)-filled])
		cyanStr := string(runes[len(runes)-filled:])
		return fmt.Sprintf("  \033[36m⟳  \033[0m\033[32m%s\033[36m%s\033[0m", rest, cyanStr)
	}

	if total <= 0 || current <= 0 {
		return fmt.Sprintf("  \033[90m↻  %s\033[0m", display)
	}

	pct := float64(current) / float64(total)
	runes := []rune(display)
	filled := int(pct * float64(len(runes)))
	if filled > len(runes) {
		filled = len(runes)
	}
	greenStr := string(runes[:filled])
	rest := string(runes[filled:])
	return fmt.Sprintf("  \033[33m↓  \033[32m%s\033[90m%s\033[0m", greenStr, rest)
}

func runBatch(entries []PlaylistEntry, format, outputDir string, threads, workers int, mediaType string, quality int,
	done *atomic.Int64, failed *atomic.Int64, total int64,
	startTime time.Time) []PlaylistEntry {

	type result struct {
		entry PlaylistEntry
		err   error
	}

	resolveQueue := make(chan PlaylistEntry, len(entries))
	downloadQueue := make(chan resolvedEntry, workers*3)
	results := make(chan result, len(entries))

	for _, e := range entries {
		resolveQueue <- e
	}
	close(resolveQueue)

	slots := make([]*slotState, workers)
	for i := range slots {
		slots[i] = &slotState{tp: &TrackProgress{}}
	}

	var mu sync.Mutex

	render := func() {
		d := done.Load()
		f := failed.Load()

		width := 30
		filled := 0
		pct := 0
		if total > 0 {
			pct = int(float64(d) / float64(total) * 100)
			filled = int(float64(d) / float64(total) * float64(width))
			if filled > width {
				filled = width
			}
		}
		bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
		elapsed := time.Since(startTime).Round(time.Second)
		remaining := total - d - f

		failStr := ""
		if f > 0 {
			failStr = fmt.Sprintf("  \033[31m·  %d failed\033[0m", f)
		}

		fmt.Printf("\r\033[K  \033[36m[%s]\033[0m  \033[97m%d / %d\033[0m  \033[90m(%d%%)\033[0m\n", bar, d, total, pct)

		for i := 0; i < workers; i++ {
			s := slots[i]

			if s.sliding {
				s.slideOffset += 4
				if s.slideOffset >= titleWidth {
					s.sliding = false
					s.slideOffset = titleWidth
				}
			}

			line := renderTitle(s)
			fmt.Printf("\033[K%s\n", line)
		}

		fmt.Printf("\033[K  \033[90m%d left  ·  %s%s\033[0m", remaining, elapsed, failStr)
		fmt.Printf("\033[%dA\r", workers+1)
	}

	const resolvers = 8
	var resolveWg sync.WaitGroup
	for i := 0; i < resolvers; i++ {
		resolveWg.Add(1)
		go func() {
			defer resolveWg.Done()
			for entry := range resolveQueue {
				info, err := resolve(entry.WebpageURL, mediaType, quality)
				downloadQueue <- resolvedEntry{entry: entry, info: info, err: err}
			}
		}()
	}

	go func() {
		resolveWg.Wait()
		close(downloadQueue)
	}()

	var dlWg sync.WaitGroup
	for i := 0; i < workers; i++ {
		dlWg.Add(1)
		slotIdx := i
		go func() {
			defer dlWg.Done()
			for re := range downloadQueue {
				s := slots[slotIdx]
				tp := s.tp

				mu.Lock()
				if tp.Title != "" && !tp.Failed.Load() {
					s.oldTitle = tp.Title
					s.oldFailed = tp.Failed.Load()
					s.sliding = true
					s.slideOffset = 0
				}
				mu.Unlock()

				if re.err != nil {
					tp.Reset(re.entry.Title, 0)
					tp.Failed.Store(true)
					results <- result{entry: re.entry, err: fmt.Errorf("could not resolve URL: %w", re.err)}
					continue
				}

				tp.Reset(re.info.Title, re.info.Filesize)
				if re.info.FilesizeApprox > 0 && re.info.Filesize == 0 {
					tp.Total.Store(re.info.FilesizeApprox)
				}

			err := downloadResolved(re.info, re.entry.WebpageURL, format, outputDir, threads, tp, mediaType, quality)
			if err != nil {
				tp.Failed.Store(true)
			} else {
				tp.Done.Store(true)
			}
			results <- result{entry: re.entry, err: err}
			}
		}()
	}

	go func() {
		dlWg.Wait()
		close(results)
	}()

	stopRender := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopRender:
				return
			case <-time.After(80 * time.Millisecond):
				mu.Lock()
				render()
				mu.Unlock()
			}
		}
	}()

	var failedEntries []PlaylistEntry
	for r := range results {
		mu.Lock()
		if r.err != nil {
			failed.Add(1)
			failedEntries = append(failedEntries, r.entry)
		} else {
			done.Add(1)
		}
		mu.Unlock()
	}

	close(stopRender)
	return failedEntries
}

func downloadResolved(info *VideoInfo, originalURL, format, outputDir string, threads int, tp *TrackProgress, mediaType string, quality int) error {
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("cannot create output dir: %w", err)
	}

	safeTitle := sanitizeFilename(info.Title)
	outExt := format
	if mediaType == "video" {
		outExt = "mp4"
	}
	outFile := filepath.Join(outputDir, safeTitle+"."+outExt)

	if info.URL != "" {
		size := info.Filesize
		if size == 0 {
			size = info.FilesizeApprox
		}
		if tp != nil {
			tp.Total.Store(size)
		}

		tmpFile := filepath.Join(outputDir, safeTitle+".tmp")
		_, supportsRange, _ := probeURL(info.URL)

		if supportsRange && size > 0 && threads > 1 {
			if err := parallelDownload(info.URL, tmpFile, size, threads, true, tp); err != nil {
				os.Remove(tmpFile)
				return fmt.Errorf("download failed: %w", err)
			}
		} else {
			if err := streamDownload(info.URL, tmpFile, size, true, tp); err != nil {
				os.Remove(tmpFile)
				return fmt.Errorf("download failed: %w", err)
			}
		}

		if mediaType == "video" {
			if err := os.Rename(tmpFile, outFile); err != nil {
				return fmt.Errorf("rename failed: %w", err)
			}
		} else if format != info.Ext {
			if tp != nil {
				tp.Converting.Store(true)
				tp.ConvertPct.Store(3)
			}
			if err := convertAudioProgress(tmpFile, outFile, format, info.Duration, tp); err != nil {
				os.Remove(tmpFile)
				if tp != nil {
					tp.Converting.Store(false)
				}
				return fmt.Errorf("conversion failed: %w", err)
			}
			// Don't clear Converting here — runBatch will set Done
			// (higher render priority) and tp.Reset clears it for next track.
			os.Remove(tmpFile)
		} else {
			if err := os.Rename(tmpFile, outFile); err != nil {
				return fmt.Errorf("rename failed: %w", err)
			}
		}

		if format == "mp3" && mediaType != "video" {
			_ = writeMP3Tags(outFile, info)
		}

	} else {
		if err := ytDlpFallback(originalURL, format, outFile, threads, true, mediaType, quality, tp); err != nil {
			return err
		}
	}

	return nil
}

func writeMP3Tags(outFile string, info *VideoInfo) error {
	return metadata.WriteMP3Tags(outFile, info.Title, info.DisplayArtist(), info.Album, info.Thumbnail)
}

func DownloadPlaylist(playlistURL, format, outputDir string, threads, workers int, mediaType string, quality int) error {
	bold := color.New(color.FgWhite, color.Bold)
	cyan := color.New(color.FgCyan)
	gray := color.New(color.FgHiBlack)
	green := color.New(color.FgGreen, color.Bold)
	red := color.New(color.FgRed)
	yellow := color.New(color.FgYellow)

	fmt.Print("\033[?25l")
	defer fmt.Print("\033[?25h")
	fmt.Println()
	cyan.Print("  → ")
	bold.Println("Resolving playlist...")

	entries, playlistTitle, err := ResolvePlaylist(playlistURL)
	if err != nil {
		return fmt.Errorf("could not resolve playlist: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("no tracks found in playlist")
	}

	total := int64(len(entries))

	// Organize: video/ subfolder for video, playlist gets its own folder
	playlistDir := sanitizeFilename(playlistTitle)
	if mediaType == "video" {
		outputDir = filepath.Join(outputDir, "video", playlistDir)
	} else {
		outputDir = filepath.Join(outputDir, playlistDir)
	}

	fmt.Println()
	dispFormat := format
	if mediaType == "video" {
		dispFormat = "mp4"
	}
	bold.Printf("  %d tracks", total)
	gray.Printf("  ·  %d workers  ·  %d threads  ·  %s\n", workers, threads, dispFormat)
	fmt.Println()

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("cannot create output dir: %w", err)
	}

	var (
		done      atomic.Int64
		failed    atomic.Int64
		startTime = time.Now()
	)

	failedEntries := runBatch(entries, format, outputDir, threads, workers, mediaType, quality,
		&done, &failed, total, startTime)

	for len(failedEntries) > 0 {
		for i := 0; i < workers+2; i++ {
			fmt.Print("\033[1B\033[K")
		}
		fmt.Printf("\033[%dA\r", workers+2)
		fmt.Println()
		yellow.Printf("  ↻  Retrying %d failed track(s)...\n", len(failedEntries))
		fmt.Println()

		failed.Store(0)
		prev := len(failedEntries)

		failedEntries = runBatch(failedEntries, format, outputDir, threads, workers, mediaType, quality,
			&done, &failed, total, startTime)

		if len(failedEntries) >= prev {
			break
		}
	}

	for i := 0; i < workers+2; i++ {
		fmt.Print("\033[1B\033[K")
	}
	fmt.Printf("\033[%dA\r", workers+2)

	elapsed := time.Since(startTime).Round(time.Millisecond)
	fmt.Println()
	green.Printf("  ✓  %d downloaded", done.Load())
	if failed.Load() > 0 {
		red.Printf("  ·  %d failed", failed.Load())
	}
	gray.Printf("  (%s)\n", elapsed)
	fmt.Println()

	if len(failedEntries) > 0 {
		gray.Println("  failed tracks:")
		for _, t := range failedEntries {
			fmt.Printf("    · %s\n", t.Title)
		}
		fmt.Println()
	}

	return nil
}
