package metadata

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bogem/id3v2/v2"
)

const maxThumbnailSize = 5 * 1024 * 1024

var allowedHosts = map[string]bool{
	"i.ytimg.com":               true,
	"yt3.ggpht.com":             true,
	"lh3.googleusercontent.com": true,
	"i9.ytimg.com":              true,
}

func isAllowedHost(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.TrimPrefix(parsed.Host, "www.")
	return allowedHosts[host]
}

func stripQueryParams(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

// fetchImageWithValidation downloads and validates thumbnail images
func fetchImageWithValidation(rawURL string) ([]byte, error) {
	if !isAllowedHost(rawURL) {
		return nil, fmt.Errorf("thumbnail host not allowed")
	}

	url := stripQueryParams(rawURL)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", "radii5-metadata-fetcher")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("image fetch failed with status %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || !strings.HasPrefix(mediaType, "image/") {
			return nil, fmt.Errorf("invalid content type: %s", contentType)
		}
	}

	limitedReader := io.LimitReader(resp.Body, maxThumbnailSize)
	data, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("failed to read image data: %w", err)
	}

	if len(data) == maxThumbnailSize {
		return nil, fmt.Errorf("image too large (>5MB)")
	}

	return data, nil
}

// WriteMP3Tags writes ID3v2 tags to a downloaded MP3 file
func WriteMP3Tags(path, title, artist, album, thumbnailURL string) error {
	tag, err := id3v2.Open(path, id3v2.Options{Parse: true})
	if err != nil {
		return fmt.Errorf("failed to open MP3 file: %w", err)
	}
	defer tag.Close()

	if title != "" {
		tag.SetTitle(title)
	}
	if artist != "" {
		tag.SetArtist(artist)
	}
	if album != "" {
		tag.SetAlbum(album)
	}

	// Embed thumbnail as album art
	if thumbnailURL != "" {
		if img, err := fetchImageWithValidation(thumbnailURL); err == nil {
			pic := id3v2.PictureFrame{
				Encoding:    id3v2.EncodingUTF8,
				MimeType:    "image/jpeg",
				PictureType: id3v2.PTFrontCover,
				Description: "Cover",
				Picture:     img,
			}
			tag.AddAttachedPicture(pic)
		} else {
			// Log error but don't fail the entire operation
			fmt.Printf("Warning: failed to fetch thumbnail: %v\n", err)
		}
	}

	return tag.Save()
}

// fetchImage is kept for backward compatibility but deprecated
func fetchImage(url string) ([]byte, error) {
	return fetchImageWithValidation(url)
}
