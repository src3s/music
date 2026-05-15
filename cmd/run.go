package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fatih/color"
	"github.com/ohcass/music/internal/downloader"
)

type Options struct {
	URL       string
	Format    string
	OutputDir string
	Threads   int
	Workers   int
	MediaType string // "audio" or "video"
	Quality   int    // video height (0 = best available)
}

func defaultMusicDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "radii5 downloads"
	}
	return filepath.Join(home, "Music", "radii5 downloads")
}

func IsPlaylist(url string) bool {
	return strings.Contains(url, "playlist?list=") ||
		strings.Contains(url, "/sets/") || // SoundCloud playlists
		strings.Contains(url, "/album/") // Bandcamp albums
}

func Run(args []string) {
	opts := ParseArgs(args)
	if opts.URL == "" {
		color.Red("  ✗ No URL provided")
		fmt.Println("  Usage: radii5 <url>")
		os.Exit(1)
	}

	if IsPlaylist(opts.URL) {
		if err := downloader.DownloadPlaylist(opts.URL, opts.Format, opts.OutputDir, opts.Threads, opts.Workers, opts.MediaType, opts.Quality); err != nil {
			color.Red("✗ %v", err)
			os.Exit(1)
		}
	} else {
		if err := downloader.Download(opts.URL, opts.Format, opts.OutputDir, opts.Threads, false, nil, opts.MediaType, opts.Quality); err != nil {
			color.Red("✗ %v", err)
			os.Exit(1)
		}
	}
}

func ParseArgs(args []string) Options {
	opts := Options{
		Format:    "mp3",
		OutputDir: defaultMusicDir(),
		Threads:   8,
		Workers:   4,
		MediaType: "audio",
		Quality:   1080,
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--format", "-f":
			if i+1 < len(args) {
				i++
				opts.Format = args[i]
			}
		case "--output", "-o":
			if i+1 < len(args) {
				i++
				opts.OutputDir = args[i]
			}
		case "--threads", "-t":
			if i+1 < len(args) {
				i++
				if n, err := strconv.Atoi(args[i]); err == nil {
					opts.Threads = n
				}
			}
		case "--workers", "-w":
			if i+1 < len(args) {
				i++
				if n, err := strconv.Atoi(args[i]); err == nil {
					opts.Workers = n
				}
			}
		case "--type":
			if i+1 < len(args) {
				i++
				opts.MediaType = args[i]
			}
		case "--quality", "-q":
			if i+1 < len(args) {
				i++
				if n, err := strconv.Atoi(args[i]); err == nil {
					opts.Quality = n
				}
			}
		case "-mp4":
			opts.MediaType = "video"
			if i+1 < len(args) {
				i++
				if n, err := strconv.Atoi(args[i]); err == nil {
					opts.Quality = n
				}
			}
		default:
			if opts.URL == "" && len(args[i]) > 0 && args[i][0] != '-' {
				opts.URL = args[i]
			} else if len(args[i]) > 0 && args[i][0] == '-' {
				fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			}
		}
	}
	return opts
}
