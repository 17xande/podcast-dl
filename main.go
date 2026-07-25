package main

import (
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type rssFeed struct {
	XMLName xml.Name `xml:"rss"`
	Channel struct {
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
}

type rssItem struct {
	Title     string `xml:"title"`
	PubDate   string `xml:"pubDate"`
	Enclosure struct {
		URL  string `xml:"url,attr"`
		Type string `xml:"type,attr"`
	} `xml:"enclosure"`
}

// pubDateLayouts covers the RFC 822/1123-style formats RSS feeds commonly
// use for <pubDate>, plus a couple of common variants and RFC 3339 for
// feeds that deviate from spec.
var pubDateLayouts = []string{
	time.RFC1123Z,
	time.RFC1123,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	time.RFC3339,
	"02 Jan 2006 15:04:05 -0700",
}

// parsePubDate parses an RSS <pubDate> value, returning ok=false if it's
// empty or in a format we don't recognize.
func parsePubDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range pubDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// itunesLookupURL is the iTunes Lookup API endpoint used to resolve an Apple
// Podcasts URL to its underlying RSS feed URL. It's a var so tests can point
// it at a local httptest server.
var itunesLookupURL = "https://itunes.apple.com/lookup"

var applePodcastIDPattern = regexp.MustCompile(`^id(\d+)$`)

// parseApplePodcastID extracts the numeric podcast id from an Apple Podcasts
// (or legacy iTunes) URL, e.g. https://podcasts.apple.com/us/podcast/some-show/id1200361736.
// It returns ok=false for any URL that isn't an Apple Podcasts URL.
func parseApplePodcastID(rawURL string) (id string, ok bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}

	host := strings.ToLower(u.Host)
	if !strings.HasSuffix(host, "podcasts.apple.com") && !strings.HasSuffix(host, "itunes.apple.com") {
		return "", false
	}

	for _, segment := range strings.Split(u.Path, "/") {
		if m := applePodcastIDPattern.FindStringSubmatch(segment); m != nil {
			return m[1], true
		}
	}
	return "", false
}

type itunesLookupResponse struct {
	Results []struct {
		FeedURL string `json:"feedUrl"`
	} `json:"results"`
}

// resolveFeedURL takes a URL that may either already be an RSS feed or an
// Apple Podcasts page URL, and returns the RSS feed URL to use. Non-Apple
// URLs are returned unchanged.
func resolveFeedURL(client *http.Client, rawURL string) (string, error) {
	id, ok := parseApplePodcastID(rawURL)
	if !ok {
		return rawURL, nil
	}

	lookupURL := fmt.Sprintf("%s?id=%s&entity=podcast", itunesLookupURL, id)
	resp, err := client.Get(lookupURL)
	if err != nil {
		return "", fmt.Errorf("resolving Apple Podcasts URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolving Apple Podcasts URL: unexpected status %s", resp.Status)
	}

	var lookup itunesLookupResponse
	if err := json.NewDecoder(resp.Body).Decode(&lookup); err != nil {
		return "", fmt.Errorf("resolving Apple Podcasts URL: %w", err)
	}

	if len(lookup.Results) == 0 || lookup.Results[0].FeedURL == "" {
		return "", fmt.Errorf("resolving Apple Podcasts URL: no feed found for podcast id %s", id)
	}

	return lookup.Results[0].FeedURL, nil
}

func parseFeed(r io.Reader) (rssFeed, error) {
	var feed rssFeed
	if err := xml.NewDecoder(r).Decode(&feed); err != nil {
		return rssFeed{}, fmt.Errorf("parsing feed: %w", err)
	}
	return feed, nil
}

// selectEpisodes returns the items to download: just the first item with an
// enclosure unless all is true, in which case every item with an enclosure
// is returned.
func selectEpisodes(feed rssFeed, all bool) []rssItem {
	var selected []rssItem
	for _, item := range feed.Channel.Items {
		if item.Enclosure.URL == "" {
			continue
		}
		selected = append(selected, item)
		if !all {
			break
		}
	}
	return selected
}

var reservedChars = regexp.MustCompile(`[/\\:*?"<>|\x00-\x1f]`)
var whitespaceRun = regexp.MustCompile(`\s+`)

// sanitizeFilename converts a podcast episode title into a name safe to use
// as a file on disk.
func sanitizeFilename(title string) string {
	name := reservedChars.ReplaceAllString(title, "-")
	name = whitespaceRun.ReplaceAllString(name, " ")
	name = strings.Trim(name, " .")
	if name == "" {
		return "episode"
	}
	return name
}

// episodeExtension derives a file extension for an episode from its
// enclosure URL, falling back to .mp3 if none can be determined.
func episodeExtension(enclosureURL string) string {
	ext := path.Ext(strings.SplitN(enclosureURL, "?", 2)[0])
	if ext == "" {
		return ".mp3"
	}
	return ext
}

// destinationPath builds the file path an episode should be saved to,
// prefixing the sanitized title with its release date (YYYY-MM-DD) when the
// feed provides a parseable pubDate.
func destinationPath(outDir string, item rssItem) string {
	name := sanitizeFilename(item.Title)
	if t, ok := parsePubDate(item.PubDate); ok {
		name = t.Format("2006-01-02") + " - " + name
	}
	return filepath.Join(outDir, name+episodeExtension(item.Enclosure.URL))
}

// formatBytes renders a byte count in human-readable form, e.g. "4.2 MiB".
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// progressLine renders a download progress status, including a percentage
// when the total size is known.
func progressLine(written, total int64) string {
	if total > 0 {
		pct := float64(written) / float64(total) * 100
		return fmt.Sprintf("%s / %s (%.0f%%)", formatBytes(written), formatBytes(total), pct)
	}
	return formatBytes(written)
}

// progressWriter is an io.Writer that prints download progress to stdout as
// bytes flow through it, throttled so it doesn't flood the terminal.
type progressWriter struct {
	total     int64
	written   int64
	lastPrint time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.written += int64(len(b))
	if now := time.Now(); now.Sub(p.lastPrint) >= 150*time.Millisecond {
		p.print()
		p.lastPrint = now
	}
	return len(b), nil
}

func (p *progressWriter) print() {
	fmt.Printf("\r  %s", progressLine(p.written, p.total))
}

func (p *progressWriter) finish() {
	p.print()
	fmt.Println()
}

// downloadEpisode fetches the enclosure audio for item and writes it to
// destPath, unless a file already exists there. When pubTime is known, the
// downloaded file's access/modification times are set to the episode's
// release date.
func downloadEpisode(client *http.Client, item rssItem, destPath string, pubTime time.Time, hasPubTime bool) error {
	if _, err := os.Stat(destPath); err == nil {
		fmt.Printf("skip (exists): %s\n", item.Title)
		return nil
	}

	resp, err := client.Get(item.Enclosure.URL)
	if err != nil {
		return fmt.Errorf("downloading %q: %w", item.Title, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %q: unexpected status %s", item.Title, resp.Status)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("creating file for %q: %w", item.Title, err)
	}
	defer out.Close()

	fmt.Printf("downloading: %s\n", item.Title)
	progress := &progressWriter{total: resp.ContentLength}
	if _, err := io.Copy(io.MultiWriter(out, progress), resp.Body); err != nil {
		fmt.Println()
		return fmt.Errorf("saving %q: %w", item.Title, err)
	}
	progress.finish()

	if hasPubTime {
		if err := os.Chtimes(destPath, pubTime, pubTime); err != nil {
			return fmt.Errorf("setting file date for %q: %w", item.Title, err)
		}
		if err := setCreationTime(destPath, pubTime); err != nil {
			return fmt.Errorf("setting file creation date for %q: %w", item.Title, err)
		}
	}

	return nil
}

func run(inputURL, outDir string, all bool) error {
	client := &http.Client{}

	feedURL, err := resolveFeedURL(client, inputURL)
	if err != nil {
		return err
	}

	resp, err := client.Get(feedURL)
	if err != nil {
		return fmt.Errorf("fetching feed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching feed: unexpected status %s", resp.Status)
	}

	feed, err := parseFeed(resp.Body)
	if err != nil {
		return err
	}

	episodes := selectEpisodes(feed, all)
	if len(episodes) == 0 {
		return fmt.Errorf("no episodes with downloadable audio found in feed")
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	for _, item := range episodes {
		dest := destinationPath(outDir, item)
		pubTime, hasPubTime := parsePubDate(item.PubDate)
		if err := downloadEpisode(client, item, dest, pubTime, hasPubTime); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
	}

	return nil
}

func main() {
	url := flag.String("url", "", "podcast RSS feed URL, or an Apple Podcasts URL (required)")
	all := flag.Bool("all", false, "download all episodes instead of just the latest")
	out := flag.String("out", ".", "directory to save downloaded episodes into")
	flag.Parse()

	if *url == "" {
		fmt.Fprintln(os.Stderr, "error: -url is required")
		flag.Usage()
		os.Exit(1)
	}

	if err := run(*url, *out, *all); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
