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
)

type rssFeed struct {
	XMLName xml.Name `xml:"rss"`
	Channel struct {
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
}

type rssItem struct {
	Title     string `xml:"title"`
	Enclosure struct {
		URL  string `xml:"url,attr"`
		Type string `xml:"type,attr"`
	} `xml:"enclosure"`
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

func destinationPath(outDir string, item rssItem) string {
	name := sanitizeFilename(item.Title) + episodeExtension(item.Enclosure.URL)
	return filepath.Join(outDir, name)
}

// downloadEpisode fetches the enclosure audio for item and writes it to
// destPath, unless a file already exists there.
func downloadEpisode(client *http.Client, item rssItem, destPath string) error {
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

	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("saving %q: %w", item.Title, err)
	}

	fmt.Printf("downloaded: %s\n", item.Title)
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
		if err := downloadEpisode(client, item, dest); err != nil {
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
