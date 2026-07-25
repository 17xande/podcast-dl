package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSanitizeFilename(t *testing.T) {
	cases := []struct {
		name  string
		title string
		want  string
	}{
		{"reserved characters", `Ep 1: "Weird" / Title? <A> *B* | C\D`, "Ep 1- -Weird- - Title- -A- -B- - C-D"},
		{"control characters", "Title\x00With\x1fControl", "Title-With-Control"},
		{"repeated whitespace", "Too    much   space", "Too much space"},
		{"leading and trailing dots and spaces", "  ...Title...  ", "Title"},
		{"empty title falls back", "", "episode"},
		{"only reserved characters become dashes", "///???", "------"},
		{"plain title unchanged", "Episode 42 - The Finale", "Episode 42 - The Finale"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeFilename(c.title)
			if got != c.want {
				t.Errorf("sanitizeFilename(%q) = %q, want %q", c.title, got, c.want)
			}
			if got == "" {
				t.Errorf("sanitizeFilename(%q) returned empty string", c.title)
			}
		})
	}
}

func TestParseApplePodcastID(t *testing.T) {
	cases := []struct {
		name   string
		url    string
		wantID string
		wantOK bool
	}{
		{"standard podcast page", "https://podcasts.apple.com/us/podcast/some-show/id1200361736", "1200361736", true},
		{"with episode query param", "https://podcasts.apple.com/us/podcast/some-show/id1200361736?i=1000123456789", "1200361736", true},
		{"legacy itunes host", "https://itunes.apple.com/us/podcast/some-show/id1200361736?mt=2", "1200361736", true},
		{"no country segment", "https://podcasts.apple.com/podcast/id1200361736", "1200361736", true},
		{"plain rss feed url", "https://example.com/feed.xml", "", false},
		{"apple.com but not podcasts host", "https://music.apple.com/us/album/id1200361736", "", false},
		{"malformed url", "://bad-url", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, ok := parseApplePodcastID(c.url)
			if ok != c.wantOK || id != c.wantID {
				t.Errorf("parseApplePodcastID(%q) = (%q, %v), want (%q, %v)", c.url, id, ok, c.wantID, c.wantOK)
			}
		})
	}
}

func TestResolveFeedURL(t *testing.T) {
	t.Run("non-apple url returned unchanged without network call", func(t *testing.T) {
		got, err := resolveFeedURL(http.DefaultClient, "https://example.com/feed.xml")
		if err != nil {
			t.Fatalf("resolveFeedURL returned error: %v", err)
		}
		if got != "https://example.com/feed.xml" {
			t.Errorf("got %q, want unchanged URL", got)
		}
	})

	t.Run("apple podcast url resolved via lookup api", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("id"); got != "1200361736" {
				t.Errorf("lookup called with id=%q, want 1200361736", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"results":[{"feedUrl":"https://example.com/real-feed.xml"}]}`))
		}))
		defer server.Close()

		origURL := itunesLookupURL
		itunesLookupURL = server.URL
		defer func() { itunesLookupURL = origURL }()

		got, err := resolveFeedURL(server.Client(), "https://podcasts.apple.com/us/podcast/some-show/id1200361736")
		if err != nil {
			t.Fatalf("resolveFeedURL returned error: %v", err)
		}
		if got != "https://example.com/real-feed.xml" {
			t.Errorf("got %q, want %q", got, "https://example.com/real-feed.xml")
		}
	})

	t.Run("no results returns error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"results":[]}`))
		}))
		defer server.Close()

		origURL := itunesLookupURL
		itunesLookupURL = server.URL
		defer func() { itunesLookupURL = origURL }()

		_, err := resolveFeedURL(server.Client(), "https://podcasts.apple.com/us/podcast/some-show/id1200361736")
		if err == nil {
			t.Fatal("expected error when lookup returns no results, got nil")
		}
	})
}

func TestParsePubDate(t *testing.T) {
	want := time.Date(2026, time.July, 25, 10, 30, 0, 0, time.FixedZone("", -4*3600))

	cases := []struct {
		name   string
		input  string
		wantOK bool
	}{
		{"RFC1123Z", "Sat, 25 Jul 2026 10:30:00 -0400", true},
		{"RFC1123 with zone name", "Sat, 25 Jul 2026 10:30:00 EDT", true},
		{"single digit day", "Sat, 5 Jul 2026 10:30:00 -0400", true},
		{"RFC3339", "2026-07-25T10:30:00-04:00", true},
		{"no weekday", "25 Jul 2026 10:30:00 -0400", true},
		{"empty string", "", false},
		{"garbage", "not a date", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, ok := parsePubDate(c.input)
			if ok != c.wantOK {
				t.Errorf("parsePubDate(%q) ok = %v, want %v", c.input, ok, c.wantOK)
			}
		})
	}

	t.Run("parses correct instant", func(t *testing.T) {
		got, ok := parsePubDate("Sat, 25 Jul 2026 10:30:00 -0400")
		if !ok {
			t.Fatal("expected ok=true")
		}
		if !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

func TestSetCreationTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.mp3")
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatalf("setup: writing file: %v", err)
	}

	if err := setCreationTime(path, time.Date(2026, time.July, 25, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Errorf("setCreationTime returned error: %v", err)
	}
}

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{500, "500 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1048576, "1.0 MiB"},
		{5 * 1048576, "5.0 MiB"},
	}
	for _, c := range cases {
		if got := formatBytes(c.n); got != c.want {
			t.Errorf("formatBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestProgressLine(t *testing.T) {
	cases := []struct {
		written, total int64
		want           string
	}{
		{512, 1024, "512 B / 1.0 KiB (50%)"},
		{1024, 1024, "1.0 KiB / 1.0 KiB (100%)"},
		{2048, -1, "2.0 KiB"},
		{2048, 0, "2.0 KiB"},
	}
	for _, c := range cases {
		if got := progressLine(c.written, c.total); got != c.want {
			t.Errorf("progressLine(%d, %d) = %q, want %q", c.written, c.total, got, c.want)
		}
	}
}

func TestParseFeed(t *testing.T) {
	t.Run("multiple items", func(t *testing.T) {
		xmlBody := `<?xml version="1.0"?>
<rss version="2.0"><channel>
<title>Test Podcast</title>
<item><title>Episode One</title><enclosure url="https://example.com/ep1.mp3" type="audio/mpeg"/></item>
<item><title>Episode Two</title><enclosure url="https://example.com/ep2.mp3" type="audio/mpeg"/></item>
<item><title>No Audio Episode</title></item>
</channel></rss>`

		feed, err := parseFeed(strings.NewReader(xmlBody))
		if err != nil {
			t.Fatalf("parseFeed returned error: %v", err)
		}
		if len(feed.Channel.Items) != 3 {
			t.Fatalf("expected 3 items, got %d", len(feed.Channel.Items))
		}
		if feed.Channel.Items[0].Title != "Episode One" {
			t.Errorf("item 0 title = %q, want %q", feed.Channel.Items[0].Title, "Episode One")
		}
		if feed.Channel.Items[0].Enclosure.URL != "https://example.com/ep1.mp3" {
			t.Errorf("item 0 enclosure URL = %q", feed.Channel.Items[0].Enclosure.URL)
		}
		if feed.Channel.Items[2].Enclosure.URL != "" {
			t.Errorf("item 2 (no audio) should have empty enclosure URL, got %q", feed.Channel.Items[2].Enclosure.URL)
		}
	})

	t.Run("malformed xml returns error", func(t *testing.T) {
		_, err := parseFeed(strings.NewReader("<rss><channel><item>"))
		if err == nil {
			t.Fatal("expected error for malformed XML, got nil")
		}
	})
}

func TestSelectEpisodes(t *testing.T) {
	feed := rssFeed{}
	feed.Channel.Items = []rssItem{
		{Title: "Newest"},
		{Title: "Middle"},
		{Title: "Oldest"},
	}
	feed.Channel.Items[0].Enclosure.URL = "https://example.com/newest.mp3"
	feed.Channel.Items[1].Enclosure.URL = "https://example.com/middle.mp3"
	feed.Channel.Items[2].Enclosure.URL = "https://example.com/oldest.mp3"

	t.Run("without all, only first item with enclosure", func(t *testing.T) {
		got := selectEpisodes(feed, false)
		if len(got) != 1 {
			t.Fatalf("expected 1 item, got %d", len(got))
		}
		if got[0].Title != "Newest" {
			t.Errorf("got title %q, want %q", got[0].Title, "Newest")
		}
	})

	t.Run("with all, every item with an enclosure", func(t *testing.T) {
		got := selectEpisodes(feed, true)
		if len(got) != 3 {
			t.Fatalf("expected 3 items, got %d", len(got))
		}
	})

	t.Run("skips items without enclosure", func(t *testing.T) {
		f := rssFeed{}
		f.Channel.Items = []rssItem{
			{Title: "No Audio"},
			{Title: "Has Audio"},
		}
		f.Channel.Items[1].Enclosure.URL = "https://example.com/has-audio.mp3"

		got := selectEpisodes(f, true)
		if len(got) != 1 || got[0].Title != "Has Audio" {
			t.Fatalf("expected only 'Has Audio' item, got %+v", got)
		}
	})

	t.Run("no items with enclosure returns empty", func(t *testing.T) {
		f := rssFeed{}
		f.Channel.Items = []rssItem{{Title: "No Audio"}}
		got := selectEpisodes(f, true)
		if len(got) != 0 {
			t.Fatalf("expected 0 items, got %d", len(got))
		}
	})
}

func TestDestinationPath(t *testing.T) {
	dir := t.TempDir()

	t.Run("prefixes with release date when pubDate parses", func(t *testing.T) {
		item := rssItem{Title: "Episode Title", PubDate: "Sat, 25 Jul 2026 10:30:00 -0400"}
		item.Enclosure.URL = "https://example.com/ep.mp3"

		got := destinationPath(dir, item)
		want := filepath.Join(dir, "2026-07-25 - Episode Title.mp3")
		if got != want {
			t.Errorf("destinationPath = %q, want %q", got, want)
		}
	})

	t.Run("no date prefix when pubDate missing or invalid", func(t *testing.T) {
		item := rssItem{Title: "Episode Title", PubDate: "not a date"}
		item.Enclosure.URL = "https://example.com/ep.mp3"

		got := destinationPath(dir, item)
		want := filepath.Join(dir, "Episode Title.mp3")
		if got != want {
			t.Errorf("destinationPath = %q, want %q", got, want)
		}
	})
}

func TestEpisodeExtension(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://example.com/audio/episode1.mp3", ".mp3"},
		{"https://example.com/audio/episode1.m4a?token=abc123", ".m4a"},
		{"https://example.com/audio/episode-with-no-extension", ".mp3"},
		{"https://example.com/audio/episode1.MP3", ".MP3"},
	}

	for _, c := range cases {
		got := episodeExtension(c.url)
		if got != c.want {
			t.Errorf("episodeExtension(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

func TestDownloadEpisode(t *testing.T) {
	t.Run("downloads via http and writes sanitized file", func(t *testing.T) {
		const audioBody = "fake audio bytes"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(audioBody))
		}))
		defer server.Close()

		dir := t.TempDir()
		item := rssItem{Title: `Episode: "One"/Two`}
		item.Enclosure.URL = server.URL + "/ep1.mp3"

		dest := destinationPath(dir, item)
		wantName := filepath.Join(dir, `Episode- -One--Two.mp3`)
		if dest != wantName {
			t.Fatalf("destinationPath = %q, want %q", dest, wantName)
		}

		if err := downloadEpisode(server.Client(), item, dest, time.Time{}, false); err != nil {
			t.Fatalf("downloadEpisode returned error: %v", err)
		}

		got, err := os.ReadFile(dest)
		if err != nil {
			t.Fatalf("reading downloaded file: %v", err)
		}
		if string(got) != audioBody {
			t.Errorf("file contents = %q, want %q", got, audioBody)
		}
	})

	t.Run("sets file times to the episode release date", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("audio bytes"))
		}))
		defer server.Close()

		dir := t.TempDir()
		item := rssItem{Title: "Dated Episode", PubDate: "Sat, 25 Jul 2026 10:30:00 -0400"}
		item.Enclosure.URL = server.URL + "/ep.mp3"
		dest := destinationPath(dir, item)

		pubTime, ok := parsePubDate(item.PubDate)
		if !ok {
			t.Fatal("setup: expected pubDate to parse")
		}

		if err := downloadEpisode(server.Client(), item, dest, pubTime, true); err != nil {
			t.Fatalf("downloadEpisode returned error: %v", err)
		}

		info, err := os.Stat(dest)
		if err != nil {
			t.Fatalf("stat downloaded file: %v", err)
		}
		if !info.ModTime().Equal(pubTime) {
			t.Errorf("file mod time = %v, want %v", info.ModTime(), pubTime)
		}
	})

	t.Run("skips download when file already exists", func(t *testing.T) {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("should not be fetched"))
		}))
		defer server.Close()

		dir := t.TempDir()
		item := rssItem{Title: "Already Here"}
		item.Enclosure.URL = server.URL + "/ep.mp3"
		dest := filepath.Join(dir, "Already Here.mp3")

		if err := os.WriteFile(dest, []byte("existing content"), 0o644); err != nil {
			t.Fatalf("setup: writing existing file: %v", err)
		}

		if err := downloadEpisode(server.Client(), item, dest, time.Time{}, false); err != nil {
			t.Fatalf("downloadEpisode returned error: %v", err)
		}

		if calls != 0 {
			t.Errorf("expected server not to be called, got %d calls", calls)
		}

		got, err := os.ReadFile(dest)
		if err != nil {
			t.Fatalf("reading file: %v", err)
		}
		if string(got) != "existing content" {
			t.Errorf("existing file was overwritten: %q", got)
		}
	})

	t.Run("http error status returns error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		dir := t.TempDir()
		item := rssItem{Title: "Missing"}
		item.Enclosure.URL = server.URL + "/missing.mp3"
		dest := filepath.Join(dir, "Missing.mp3")

		if err := downloadEpisode(server.Client(), item, dest, time.Time{}, false); err == nil {
			t.Fatal("expected error for 404 response, got nil")
		}
	})
}
