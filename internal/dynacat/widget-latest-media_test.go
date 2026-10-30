package dynacat

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGroupLatestMediaSeries(t *testing.T) {
	items := []latestMediaItem{
		{Title: "newest", MediaType: "episode", ServerType: "plex", ServerURL: "one", SeriesID: "1", SeriesLinkURL: "series"},
		{Title: "movie", MediaType: "movie", ServerType: "plex", ServerURL: "one", SeriesID: "1"},
		{Title: "older", MediaType: "episode", ServerType: "plex", ServerURL: "one", SeriesID: "1"},
		{Title: "other server", MediaType: "episode", ServerType: "plex", ServerURL: "two", SeriesID: "1"},
		{Title: "other provider", MediaType: "episode", ServerType: "emby", ServerURL: "one", SeriesID: "1"},
		{Title: "unknown one", MediaType: "episode"},
		{Title: "unknown two", MediaType: "episode"},
	}
	got := groupLatestMediaSeries(items)
	want := []string{"newest", "movie", "other server", "other provider", "unknown one", "unknown two"}
	if len(got) != len(want) {
		t.Fatalf("got %d items, want %d", len(got), len(want))
	}
	for i, title := range want {
		if got[i].Title != title {
			t.Errorf("item %d: got %q, want %q", i, got[i].Title, title)
		}
	}
	if got[0].LinkURL != "series" {
		t.Error("group must link to series")
	}
	if items[0].LinkURL != "" {
		t.Error("grouping mutated input")
	}
}

func TestLatestMediaFetchItemCount(t *testing.T) {
	for _, tc := range []struct {
		grouped     bool
		count, want int
	}{
		{false, 10, 10}, {true, 10, 100}, {true, 100, 1000}, {true, 10000, 1000},
	} {
		w := latestMediaWidget{GroupBySeries: tc.grouped, ItemCount: tc.count}
		if got := w.fetchItemCount(); got != tc.want {
			t.Errorf("%+v: got %d", tc, got)
		}
	}
}

func TestLatestMediaPlexGrouping(t *testing.T) {
	for _, grouped := range []bool{false, true} {
		t.Run(fmt.Sprint(grouped), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/identity":
					fmt.Fprint(w, `{"MediaContainer":{"machineIdentifier":"test-server"}}`)
				case "/library/sections":
					fmt.Fprint(w, `{"MediaContainer":{"Directory":[{"key":"1","type":"show","title":"TV"}]}}`)
				case "/library/sections/1/recentlyAdded":
					want := "2"
					if grouped {
						want = "20"
					}
					if r.URL.Query().Get("X-Plex-Container-Size") != want {
						t.Errorf("unexpected fetch size: %s", r.URL.RawQuery)
					}
					fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"title":"new","type":"episode","grandparentRatingKey":"42","grandparentTitle":"Show","key":"/library/metadata/1","addedAt":30},{"title":"old","type":"episode","grandparentRatingKey":"42","key":"/library/metadata/2","addedAt":20},{"title":"Movie","type":"movie","key":"/library/metadata/3","addedAt":10}]}}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			widget := &latestMediaWidget{GroupBySeries: grouped, ItemCount: 2, Hosts: []latestMediaHostConfig{{URL: "plex:" + server.URL, Token: "test", PublicURL: "https://plex.example"}}}
			if err := widget.initialize(); err != nil {
				t.Fatal(err)
			}
			widget.update(context.Background())
			if len(widget.Items) != 2 {
				t.Fatalf("got %d items", len(widget.Items))
			}
			if grouped {
				if widget.Items[1].Title != "Movie" {
					t.Error("must group before trimming")
				}
				if !strings.Contains(widget.Items[0].LinkURL, "%2Flibrary%2Fmetadata%2F42") {
					t.Errorf("not a series link: %s", widget.Items[0].LinkURL)
				}
			} else if widget.Items[1].Title != "old" {
				t.Error("default must preserve episodes")
			}
		})
	}
}

func TestLatestMediaPosterCacheStable(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "image/png")
		fmt.Fprint(w, "test image")
	}))
	defer server.Close()
	widget := &latestMediaWidget{}
	cacheDir := t.TempDir()
	widget.Providers = &widgetProviders{imageCache: newImageCache("", cacheDir)}
	widget.resolveCachedImageURL(context.Background(), server.URL+"/poster.png", false, "cover")
	// An old file must survive refreshes, unlike a short-lived custom API image.
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(filepath.Join(cacheDir, hashString(server.URL+"/poster.png")+".png"), old, old); err != nil {
		t.Fatal(err)
	}
	first := widget.resolveCachedImageURL(context.Background(), server.URL+"/poster.png", false, "cover")
	second := widget.resolveCachedImageURL(context.Background(), server.URL+"/poster.png", false, "cover")
	if first != second || !strings.HasPrefix(first, "/.cache/") || requests != 1 {
		t.Fatalf("cache unstable: %q / %q, requests=%d", first, second, requests)
	}
}

func TestLatestMediaJellyfinEmbyGrouping(t *testing.T) {
	for _, provider := range []string{"jellyfin", "emby"} {
		t.Run(provider, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/Users":
					fmt.Fprint(w, `[{"Id":"user"}]`)
				case "/Users/user/Items/Latest":
					if r.URL.Query().Get("Limit") != "20" {
						t.Errorf("unexpected candidate limit: %s", r.URL.RawQuery)
					}
					fmt.Fprint(w, `[{"Id":"1","Name":"new","Type":"Episode","SeriesId":"42","SeriesName":"Show","DateCreated":"2026-10-30T12:00:00Z"},{"Id":"2","Name":"old","Type":"Episode","SeriesId":"42","DateCreated":"2026-10-29T12:00:00Z"},{"Id":"3","Name":"Movie","Type":"Movie","DateCreated":"2026-10-28T12:00:00Z"}]`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			widget := &latestMediaWidget{GroupBySeries: true, ItemCount: 2, Hosts: []latestMediaHostConfig{{URL: provider + ":" + server.URL, Token: "test", PublicURL: "https://media.example"}}}
			if err := widget.initialize(); err != nil {
				t.Fatal(err)
			}
			widget.update(context.Background())
			if len(widget.Items) != 2 || widget.Items[0].Title != "new" || widget.Items[1].Title != "Movie" {
				t.Fatalf("unexpected grouped items: %+v", widget.Items)
			}
			if widget.Items[0].LinkURL != "https://media.example/web/index.html#!/details?id=42" {
				t.Errorf("unexpected series link: %s", widget.Items[0].LinkURL)
			}
		})
	}
}
