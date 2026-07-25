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
// appending its release date (YYYY-MM-DD) after the sanitized title when the
// feed provides a parseable pubDate.
func destinationPath(outDir string, item rssItem) string {
	name := sanitizeFilename(item.Title)
	if t, ok := parsePubDate(item.PubDate); ok {
		name = name + " - " + t.Format("2006-01-02")
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

// estimateETA estimates the remaining download time from the average
// transfer rate observed so far, returning ok=false when there isn't enough
// information yet to estimate.
func estimateETA(written, total int64, elapsed time.Duration) (time.Duration, bool) {
	if written <= 0 || total <= 0 || elapsed <= 0 {
		return 0, false
	}
	remaining := total - written
	if remaining <= 0 {
		return 0, true
	}
	rate := float64(written) / elapsed.Seconds()
	if rate <= 0 {
		return 0, false
	}
	return time.Duration(float64(remaining) / rate * float64(time.Second)).Round(time.Second), true
}

// progressLine renders a download progress status, including a percentage
// and ETA when the total size is known.
func progressLine(written, total int64, elapsed time.Duration) string {
	if total <= 0 {
		return formatBytes(written)
	}
	pct := float64(written) / float64(total) * 100
	line := fmt.Sprintf("%s / %s (%.0f%%)", formatBytes(written), formatBytes(total), pct)
	if eta, ok := estimateETA(written, total, elapsed); ok {
		line += fmt.Sprintf(" ETA %s", eta)
	}
	return line
}

// progressWriter is an io.Writer that prints download progress to stdout as
// bytes flow through it, throttled so it doesn't flood the terminal.
type progressWriter struct {
	total     int64
	written   int64
	start     time.Time
	lastPrint time.Time
}

func newProgressWriter(total int64) *progressWriter {
	return &progressWriter{total: total, start: time.Now()}
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
	fmt.Printf("\r  %s", progressLine(p.written, p.total, time.Since(p.start)))
}

func (p *progressWriter) finish() {
	p.print()
	fmt.Println()
}

// batchETA estimates the time remaining to finish a batch of downloads, based
// on the average duration of the episodes downloaded so far.
func batchETA(avgPerEpisode time.Duration, remaining int) time.Duration {
	return (avgPerEpisode * time.Duration(remaining)).Round(time.Second)
}

// episodeHeader renders the line printed before an episode's progress bar,
// including its position in the batch (e.g. "[3/25]") when more than one
// episode is being downloaded, and an ETA for the remaining batch once one
// has been estimated.
func episodeHeader(title string, index, total int, eta time.Duration, hasETA bool) string {
	line := fmt.Sprintf("downloading: %s", title)
	if total > 1 {
		line = fmt.Sprintf("[%d/%d] downloading: %s", index, total, title)
	}
	if hasETA {
		line += fmt.Sprintf(" — ETA %s", eta)
	}
	return line
}

// downloadEpisode fetches the enclosure audio for item and writes it to
// destPath, unless a file already exists there. index and total describe
// this episode's position within the current batch; eta/hasETA (when set)
// show an estimate of how long the remaining batch will take. When pubTime
// is known, the downloaded file's access/modification times are set to the
// episode's release date. It returns whether a download actually happened
// (false when skipped) and how long it took, so callers can track the
// running average used to compute later batch ETAs.
func downloadEpisode(client *http.Client, item rssItem, destPath string, pubTime time.Time, hasPubTime bool, index, total int, eta time.Duration, hasETA bool) (downloaded bool, elapsed time.Duration, err error) {
	if _, statErr := os.Stat(destPath); statErr == nil {
		fmt.Printf("skip (exists): %s\n", item.Title)
		return false, 0, nil
	}

	resp, err := client.Get(item.Enclosure.URL)
	if err != nil {
		return false, 0, fmt.Errorf("downloading %q: %w", item.Title, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, 0, fmt.Errorf("downloading %q: unexpected status %s", item.Title, resp.Status)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return false, 0, fmt.Errorf("creating file for %q: %w", item.Title, err)
	}

	fmt.Println(episodeHeader(item.Title, index, total, eta, hasETA))
	progress := newProgressWriter(resp.ContentLength)
	if _, err := io.Copy(io.MultiWriter(out, progress), resp.Body); err != nil {
		out.Close()
		fmt.Println()
		return false, 0, fmt.Errorf("saving %q: %w", item.Title, err)
	}
	progress.finish()

	// Close before touching file metadata below: on Windows, an open
	// handle blocks Chtimes/setCreationTime with a sharing violation.
	if err := out.Close(); err != nil {
		return false, 0, fmt.Errorf("closing file for %q: %w", item.Title, err)
	}

	if hasPubTime {
		if err := os.Chtimes(destPath, pubTime, pubTime); err != nil {
			return false, 0, fmt.Errorf("setting file date for %q: %w", item.Title, err)
		}
		if err := setCreationTime(destPath, pubTime); err != nil {
			return false, 0, fmt.Errorf("setting file creation date for %q: %w", item.Title, err)
		}
	}

	return true, time.Since(progress.start), nil
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

	total := len(episodes)
	var completed int
	var totalElapsed time.Duration
	for i, item := range episodes {
		dest := destinationPath(outDir, item)
		pubTime, hasPubTime := parsePubDate(item.PubDate)

		var eta time.Duration
		hasETA := completed > 0
		if hasETA {
			eta = batchETA(totalElapsed/time.Duration(completed), total-i)
		}

		downloaded, elapsed, err := downloadEpisode(client, item, dest, pubTime, hasPubTime, i+1, total, eta, hasETA)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			continue
		}
		if downloaded {
			completed++
			totalElapsed += elapsed
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
