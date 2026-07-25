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
	"sync"
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

// progressWriter is an io.Writer that reports download progress as bytes
// flow through it, throttled so it doesn't flood the display.
type progressWriter struct {
	total     int64
	written   int64
	start     time.Time
	lastPrint time.Time
	onUpdate  func(status string)
}

func newProgressWriter(total int64, onUpdate func(status string)) *progressWriter {
	return &progressWriter{total: total, start: time.Now(), onUpdate: onUpdate}
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.written += int64(len(b))
	if now := time.Now(); now.Sub(p.lastPrint) >= 150*time.Millisecond {
		p.onUpdate(progressLine(p.written, p.total, time.Since(p.start)))
		p.lastPrint = now
	}
	return len(b), nil
}

// ceilDiv computes ceil(a/b), treating a non-positive b as no-op division.
func ceilDiv(a, b int) int {
	if b <= 0 {
		return a
	}
	return (a + b - 1) / b
}

// batchETA estimates the time remaining to finish a batch of downloads from
// the average duration of episodes downloaded so far, accounting for up to
// concurrency episodes progressing at once.
func batchETA(avgPerEpisode time.Duration, remaining, concurrency int) time.Duration {
	return (avgPerEpisode * time.Duration(ceilDiv(remaining, concurrency))).Round(time.Second)
}

// episodeLine renders a single episode's line in the live progress display,
// e.g. "[3/25] Episode Title — 12.3 MiB / 45.6 MiB (27%) ETA 8s".
func episodeLine(index, total int, title, status string) string {
	line := fmt.Sprintf("[%d/%d] %s", index, total, title)
	if status != "" {
		line += " — " + status
	}
	return line
}

// summaryLine renders the final report printed after all downloads finish.
func summaryLine(downloaded int, totalBytes int64, elapsed time.Duration, errCount int) string {
	line := fmt.Sprintf("Downloaded %d episode(s), %s, in %s", downloaded, formatBytes(totalBytes), elapsed.Round(time.Second))
	if errCount > 0 {
		line += fmt.Sprintf(" (%d error(s))", errCount)
	}
	return line
}

// isTerminal reports whether f appears to be an interactive terminal, as
// opposed to a redirected file or pipe.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// liveDisplay renders a fixed-size in-place status region (one header line
// plus one line per concurrent download slot) using ANSI cursor movement,
// so only currently active downloads are visible at once. Log lines (used
// in verbose mode, and for errors) are printed above the region as normal
// scrollback. When enabled is false (e.g. output isn't a terminal), all
// live redraws are suppressed and only Log lines are printed.
type liveDisplay struct {
	mu      sync.Mutex
	header  string
	slots   []string
	started bool
	enabled bool
}

func newLiveDisplay(slots int, enabled bool) *liveDisplay {
	return &liveDisplay{slots: make([]string, slots), enabled: enabled}
}

func (d *liveDisplay) lineCount() int { return len(d.slots) + 1 }

// moveToTop moves the cursor to the region's first line, if it's already
// been drawn once.
func (d *liveDisplay) moveToTop() {
	if d.started {
		fmt.Printf("\x1b[%dA", d.lineCount())
	}
}

// writeRegion (re)draws the header and every slot, each on its own cleared
// line, assuming the cursor is already positioned at the region's top.
func (d *liveDisplay) writeRegion() {
	fmt.Printf("\r\x1b[2K%s\n", d.header)
	for _, s := range d.slots {
		fmt.Printf("\r\x1b[2K%s\n", s)
	}
	d.started = true
}

func (d *liveDisplay) redraw() {
	if !d.enabled {
		return
	}
	d.moveToTop()
	d.writeRegion()
}

func (d *liveDisplay) SetHeader(text string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.header = text
	d.redraw()
}

func (d *liveDisplay) SetSlot(i int, text string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.slots[i] = text
	d.redraw()
}

// Log prints a permanent line above the live region (or, when disabled,
// just prints it directly).
func (d *liveDisplay) Log(line string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.enabled {
		fmt.Println(line)
		return
	}
	d.moveToTop()
	fmt.Printf("\r\x1b[2K%s\n", line)
	d.writeRegion()
}

// Stop erases the live region entirely, leaving the cursor where the region
// used to start so subsequent output (e.g. a final summary) prints cleanly.
func (d *liveDisplay) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.enabled && d.started {
		d.moveToTop()
		for i := 0; i < d.lineCount(); i++ {
			fmt.Print("\r\x1b[2K\n")
		}
		d.moveToTop()
	}
	d.started = false
}

// episodePosition describes where an episode sits in the current batch and
// which live-display slot its progress should be rendered into.
type episodePosition struct {
	index, total, slot int
}

// downloadEpisode fetches the enclosure audio for item and writes it to
// destPath, unless a file already exists there. Progress is rendered into
// display's slot for pos.slot; in verbose mode, lifecycle events (start,
// skip, completion) are also logged as permanent lines. When pubTime is
// known, the downloaded file's access/modification times are set to the
// episode's release date. It returns whether a download actually happened
// (false when skipped) and its size/duration, so callers can track batch
// totals and the running average used for later ETAs.
func downloadEpisode(client *http.Client, item rssItem, destPath string, pubTime time.Time, hasPubTime bool, pos episodePosition, display *liveDisplay, verbose bool) (downloaded bool, bytesWritten int64, elapsed time.Duration, err error) {
	if _, statErr := os.Stat(destPath); statErr == nil {
		if verbose {
			display.Log(fmt.Sprintf("skip (exists): %s", item.Title))
		}
		return false, 0, 0, nil
	}

	if verbose {
		display.Log(episodeLine(pos.index, pos.total, "downloading: "+item.Title, ""))
	}
	display.SetSlot(pos.slot, episodeLine(pos.index, pos.total, item.Title, "connecting..."))
	defer display.SetSlot(pos.slot, "")

	resp, err := client.Get(item.Enclosure.URL)
	if err != nil {
		return false, 0, 0, fmt.Errorf("downloading %q: %w", item.Title, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, 0, 0, fmt.Errorf("downloading %q: unexpected status %s", item.Title, resp.Status)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return false, 0, 0, fmt.Errorf("creating file for %q: %w", item.Title, err)
	}

	progress := newProgressWriter(resp.ContentLength, func(status string) {
		display.SetSlot(pos.slot, episodeLine(pos.index, pos.total, item.Title, status))
	})
	if _, err := io.Copy(io.MultiWriter(out, progress), resp.Body); err != nil {
		out.Close()
		return false, 0, 0, fmt.Errorf("saving %q: %w", item.Title, err)
	}
	display.SetSlot(pos.slot, episodeLine(pos.index, pos.total, item.Title, progressLine(progress.written, progress.total, time.Since(progress.start))))

	// Close before touching file metadata below: on Windows, an open
	// handle blocks Chtimes/setCreationTime with a sharing violation.
	if err := out.Close(); err != nil {
		return false, 0, 0, fmt.Errorf("closing file for %q: %w", item.Title, err)
	}

	if hasPubTime {
		if err := os.Chtimes(destPath, pubTime, pubTime); err != nil {
			return false, 0, 0, fmt.Errorf("setting file date for %q: %w", item.Title, err)
		}
		if err := setCreationTime(destPath, pubTime); err != nil {
			return false, 0, 0, fmt.Errorf("setting file creation date for %q: %w", item.Title, err)
		}
	}

	dur := time.Since(progress.start)
	if verbose {
		display.Log(fmt.Sprintf("downloaded: %s (%s in %s)", item.Title, formatBytes(progress.written), dur.Round(time.Second)))
	}

	return true, progress.written, dur, nil
}

// clampConcurrency keeps the requested worker count sane: at least 1, and no
// more than the number of episodes there are to download.
func clampConcurrency(requested, total int) int {
	if requested < 1 {
		requested = 1
	}
	if total > 0 && requested > total {
		requested = total
	}
	return requested
}

func run(inputURL, outDir string, all bool, concurrency int, verbose bool) error {
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
	total := len(episodes)
	if total == 0 {
		return fmt.Errorf("no episodes with downloadable audio found in feed")
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	concurrency = clampConcurrency(concurrency, total)

	display := newLiveDisplay(concurrency, isTerminal(os.Stdout))
	display.SetHeader(fmt.Sprintf("0/%d done", total))
	defer display.Stop()

	type result struct {
		downloaded bool
		bytes      int64
		elapsed    time.Duration
		err        error
	}

	jobs := make(chan int)
	results := make(chan result, total)

	var wg sync.WaitGroup
	wg.Add(concurrency)
	for slot := 0; slot < concurrency; slot++ {
		go func(slot int) {
			defer wg.Done()
			for i := range jobs {
				item := episodes[i]
				dest := destinationPath(outDir, item)
				pubTime, hasPubTime := parsePubDate(item.PubDate)
				pos := episodePosition{index: i + 1, total: total, slot: slot}
				downloaded, bytesWritten, elapsed, err := downloadEpisode(client, item, dest, pubTime, hasPubTime, pos, display, verbose)
				results <- result{downloaded, bytesWritten, elapsed, err}
			}
		}(slot)
	}

	go func() {
		for i := range episodes {
			jobs <- i
		}
		close(jobs)
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	start := time.Now()
	var finished, downloadedCount, errCount int
	var totalBytes int64
	var totalDownloadTime time.Duration

	for res := range results {
		finished++
		switch {
		case res.err != nil:
			errCount++
			display.Log(fmt.Sprintf("error: %v", res.err))
		case res.downloaded:
			downloadedCount++
			totalBytes += res.bytes
			totalDownloadTime += res.elapsed
		}

		remaining := total - finished
		if downloadedCount > 0 && remaining > 0 {
			eta := batchETA(totalDownloadTime/time.Duration(downloadedCount), remaining, concurrency)
			display.SetHeader(fmt.Sprintf("%d/%d done — ETA %s", finished, total, eta))
		} else {
			display.SetHeader(fmt.Sprintf("%d/%d done", finished, total))
		}
	}

	display.Stop()
	fmt.Println(summaryLine(downloadedCount, totalBytes, time.Since(start), errCount))

	return nil
}

func main() {
	url := flag.String("url", "", "podcast RSS feed URL, or an Apple Podcasts URL (required)")
	all := flag.Bool("all", false, "download all episodes instead of just the latest")
	out := flag.String("out", ".", "directory to save downloaded episodes into")
	concurrency := flag.Int("concurrency", 4, "number of episodes to download at the same time")
	verbose := flag.Bool("verbose", false, "log every download/skip instead of only showing active downloads")
	flag.Parse()

	if *url == "" {
		fmt.Fprintln(os.Stderr, "error: -url is required")
		flag.Usage()
		os.Exit(1)
	}

	if err := run(*url, *out, *all, *concurrency, *verbose); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
