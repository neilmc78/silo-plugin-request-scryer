package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// fakeScryer answers the plugin's GraphQL operations and records the last
// addTitle input.
type fakeScryer struct {
	t        *testing.T
	added    map[string]any
	searches int
	addError string
	title    map[string]any // nil: the title is gone
}

func (f *fakeScryer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/graphql" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer ska_good" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Fatal(err)
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.Contains(body.Query, "addTitle"):
		if f.addError != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"message": f.addError}}})
			return
		}
		f.added = body.Variables["input"].(map[string]any)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"addTitle": map[string]any{"title": map[string]any{"id": "t1"}}}})
	case strings.Contains(body.Query, "triggerAcquisitionSearch"):
		f.searches++
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"triggerAcquisitionSearch": map[string]any{"id": "job"}}})
	case strings.Contains(body.Query, "title(id"):
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"title": f.title}})
	case strings.Contains(body.Query, "me {"):
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"me": map[string]any{"id": "u1"}}})
	}
}

func newTestRouter(t *testing.T, fake *fakeScryer) (*Router, *pluginv1.RouterConnection) {
	t.Helper()
	fake.t = t
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	r := New()
	r.now = func() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) }
	return r, &pluginv1.RouterConnection{Id: "c1", BaseUrl: srv.URL + "/", ApiKey: "ska_good"}
}

func TestFulfillAddsAMonitoredTitleAndSearches(t *testing.T) {
	cases := []struct {
		name      string
		request   *pluginv1.RequestDescriptor
		wantFacet string
		wantIDs   map[string]any
	}{
		{"movie", &pluginv1.RequestDescriptor{MediaType: "movie", Title: "Alien", Year: 1979, ExternalIds: map[string]string{"tmdb": "348"}},
			"MOVIE", map[string]any{"tmdbId": float64(348)}},
		{"series with tvdb", &pluginv1.RequestDescriptor{MediaType: "series", Title: "24", ExternalIds: map[string]string{"tmdb": "1973", "tvdb": "76290"}},
			"SERIES", map[string]any{"tmdbId": float64(1973), "tvdbId": "76290"}},
		{"anime", &pluginv1.RequestDescriptor{MediaType: "series", Title: "Sailor Moon", IsAnime: true, ExternalIds: map[string]string{"tvdb": "78750"}},
			"ANIME", map[string]any{"tvdbId": "78750"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeScryer{}
			r, conn := newTestRouter(t, fake)
			resp, err := r.Fulfill(context.Background(), &pluginv1.FulfillRequest{
				Request:     tc.request,
				Qualities:   []*pluginv1.RequestedQuality{{Id: "1080p"}},
				Connections: []*pluginv1.RouterConnection{conn},
			})
			if err != nil {
				t.Fatal(err)
			}
			if fake.added["facet"] != tc.wantFacet || fake.added["monitored"] != true {
				t.Fatalf("addTitle input = %v", fake.added)
			}
			for k, v := range tc.wantIDs {
				if fake.added[k] != v {
					t.Fatalf("addTitle %s = %v, want %v", k, fake.added[k], v)
				}
			}
			if fake.searches != 1 {
				t.Fatalf("searches = %d, want 1", fake.searches)
			}
			got := resp.GetTargets()
			if len(got) != 1 || got[0].GetExternalId() != "t1" || got[0].GetStatus() != statusQueued || got[0].GetQuality() != "1080p" {
				t.Fatalf("targets = %v", got)
			}
		})
	}
}

func TestFulfillFailures(t *testing.T) {
	t.Run("scryer explains the refusal: the target fails", func(t *testing.T) {
		r, conn := newTestRouter(t, &fakeScryer{addError: "no metadata for tmdb 1"})
		resp, err := r.Fulfill(context.Background(), &pluginv1.FulfillRequest{
			Request:     &pluginv1.RequestDescriptor{MediaType: "movie", Title: "X"},
			Connections: []*pluginv1.RouterConnection{conn},
		})
		if err != nil || len(resp.GetTargets()) != 1 || resp.GetTargets()[0].GetStatus() != statusFailed {
			t.Fatalf("resp = %v, err = %v", resp, err)
		}
	})
	t.Run("bad key: an error so Silo retries", func(t *testing.T) {
		r, conn := newTestRouter(t, &fakeScryer{})
		conn.ApiKey = "ska_bad"
		if _, err := r.Fulfill(context.Background(), &pluginv1.FulfillRequest{
			Request:     &pluginv1.RequestDescriptor{MediaType: "movie", Title: "X"},
			Connections: []*pluginv1.RouterConnection{conn},
		}); err == nil {
			t.Fatal("a refused key must fail the call")
		}
	})
}

func TestCheckStatusMapsScryerState(t *testing.T) {
	size := int64(1000)
	secs := 600
	cases := []struct {
		name       string
		title      map[string]any
		wantStatus string
		wantPhase  string
		wantLeft   int64
	}{
		{"gone from scryer", nil, statusFailed, "", 0},
		{"still searching", map[string]any{"id": "t1", "monitored": true, "mediaFiles": []any{}, "downloadQueueItems": []any{}}, statusQueued, "", 0},
		{"downloading", map[string]any{"id": "t1", "monitored": true, "mediaFiles": []any{}, "downloadQueueItems": []any{
			map[string]any{"displayState": "DOWNLOADING", "progressPercent": 40, "sizeBytes": size, "remainingSeconds": secs},
		}}, statusDownloading, "downloading", 600},
		{"import blocked outranks downloading", map[string]any{"id": "t1", "monitored": true, "mediaFiles": []any{}, "downloadQueueItems": []any{
			map[string]any{"displayState": "DOWNLOADING", "progressPercent": 0, "sizeBytes": size},
			map[string]any{"displayState": "IMPORT_BLOCKED", "progressPercent": 100, "sizeBytes": size},
		}}, statusDownloading, "import_blocked", 1000},
		{"seeding after import is not a download", map[string]any{"id": "t1", "monitored": true, "mediaFiles": []any{map[string]any{"id": "f1"}}, "downloadQueueItems": []any{
			map[string]any{"displayState": "IMPORTED_SEEDING", "progressPercent": 100},
		}}, statusCompleted, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, conn := newTestRouter(t, &fakeScryer{title: tc.title})
			resp, err := r.CheckStatus(context.Background(), &pluginv1.CheckStatusRequest{
				Targets:     []*pluginv1.TargetRef{{Quality: "1080p", ConnectionId: "c1", ExternalId: "t1"}},
				Connections: []*pluginv1.RouterConnection{conn},
			})
			if err != nil {
				t.Fatal(err)
			}
			st := resp.GetStatuses()[0]
			if st.GetStatus() != tc.wantStatus || st.GetProgress().GetPhase() != tc.wantPhase || st.GetProgress().GetBytesLeft() != tc.wantLeft {
				t.Fatalf("status %q phase %q left %d", st.GetStatus(), st.GetProgress().GetPhase(), st.GetProgress().GetBytesLeft())
			}
		})
	}
}

func TestTestConnectionReportsARefusedKey(t *testing.T) {
	r, conn := newTestRouter(t, &fakeScryer{})
	ok, _ := r.TestConnection(context.Background(), &pluginv1.TestConnectionRequest{Connection: conn})
	conn.ApiKey = "ska_bad"
	bad, _ := r.TestConnection(context.Background(), &pluginv1.TestConnectionRequest{Connection: conn})
	if !ok.GetOk() || bad.GetOk() {
		t.Fatalf("good key ok=%v, bad key ok=%v", ok.GetOk(), bad.GetOk())
	}
}
