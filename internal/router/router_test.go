package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/neilmc78/silo-plugin-request-scryer/internal/scryer"
	"github.com/neilmc78/silo-plugin-request-scryer/internal/tmdb"
)

func init() { scryer.SeasonPoll = time.Millisecond }

// fakeScryer answers the plugin's GraphQL operations and records what it was
// asked to change.
type fakeScryer struct {
	t         *testing.T
	added     map[string]any
	searches  int
	addError  string
	title     map[string]any // nil: the title is gone
	libs      []map[string]any
	libsErr   string // makes the libraries query fail
	libsQuery string // the last libraries query sent

	reused     bool             // addTitle answers that it found the title
	titles     []map[string]any // the library, for the lookup by TMDB id
	titlesIn   string           // the facet the library keeps them in; empty: any
	findFacets []string         // the facets the lookup asked for
	findErr    string           // makes the lookup by TMDB id fail

	collectionCalls []string // "id:monitored" sent to setCollectionMonitored
	episodeCalls    []string // ids sent to setEpisodeMonitored
	titleCalls      []string // ids sent to setTitleMonitored
	hydrateAfter    int      // title reads that answer without collections, like a title Scryer is still loading
	reads           int

	tagDefs       []string // labels already in the tag registry
	tagCreateErr  string   // makes creating a tag definition fail
	tagCreateRace bool     // another request defines the tag just first: create fails, the label exists
	tagsDown      bool     // the tag list answers 503, as when Scryer is restarting
	tagged        []string // labels sent to updateTitleTags
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
	reply := func(data map[string]any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": data}) }
	input := func() map[string]any { return body.Variables["input"].(map[string]any) }
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.Contains(body.Query, "addTitle"):
		if f.addError != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"message": f.addError}}})
			return
		}
		f.added = input()
		reply(map[string]any{"addTitle": map[string]any{"title": map[string]any{"id": "t1"}, "reusedExistingTitle": f.reused}})
	case strings.Contains(body.Query, "setCollectionMonitored"):
		f.collectionCalls = append(f.collectionCalls, fmt.Sprintf("%s:%v", input()["collectionId"], input()["monitored"]))
		reply(map[string]any{"setCollectionMonitored": map[string]any{"__typename": "SetCollectionMonitoredPayload"}})
	case strings.Contains(body.Query, "setEpisodeMonitored"):
		f.episodeCalls = append(f.episodeCalls, input()["episodeId"].(string))
		reply(map[string]any{"setEpisodeMonitored": map[string]any{"__typename": "EpisodePayload"}})
	case strings.Contains(body.Query, "setTitleMonitored"):
		f.titleCalls = append(f.titleCalls, input()["titleId"].(string))
		reply(map[string]any{"setTitleMonitored": map[string]any{"id": "t1"}})
	case strings.Contains(body.Query, "updateTitleTags"):
		for _, l := range input()["add"].([]any) {
			f.tagged = append(f.tagged, l.(string))
		}
		reply(map[string]any{"updateTitleTags": []map[string]any{{"id": "t1"}}})
	case strings.Contains(body.Query, "titleTagDefinitions") && f.tagsDown:
		w.WriteHeader(http.StatusServiceUnavailable)
	case strings.Contains(body.Query, "titleTagDefinitions"):
		defs := []map[string]string{}
		for _, l := range f.tagDefs {
			defs = append(defs, map[string]string{"label": l})
		}
		reply(map[string]any{"titleTagDefinitions": defs})
	case strings.Contains(body.Query, "createTitleTagDefinition"):
		if f.tagCreateRace {
			f.tagDefs = append(f.tagDefs, body.Variables["input"].(map[string]any)["label"].(string))
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"message": "tag already exists"}}})
			return
		}
		if f.tagCreateErr != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"message": f.tagCreateErr}}})
			return
		}
		f.tagDefs = append(f.tagDefs, input()["label"].(string))
		reply(map[string]any{"createTitleTagDefinition": map[string]any{"__typename": "TitleTagDefinitionMutationPayload"}})
	case strings.Contains(body.Query, "libraries("):
		f.libsQuery = body.Query
		if f.libsErr != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"message": f.libsErr}}})
			return
		}
		reply(map[string]any{"libraries": f.libs})
	case strings.Contains(body.Query, "SiloFind") && f.findErr != "":
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"message": f.findErr}}})
	case strings.Contains(body.Query, "SiloFind"):
		facet, _ := body.Variables["facet"].(string)
		f.findFacets = append(f.findFacets, facet)
		items := f.titles
		if items == nil || (f.titlesIn != "" && facet != f.titlesIn) {
			items = []map[string]any{}
		}
		reply(map[string]any{"titles": map[string]any{"items": items, "hasMore": false}})
	case strings.Contains(body.Query, "triggerAcquisitionSearch"):
		f.searches++
		reply(map[string]any{"triggerAcquisitionSearch": map[string]any{"id": "job"}})
	case strings.Contains(body.Query, "title(id"):
		title := f.title
		if f.reads++; title != nil && f.reads <= f.hydrateAfter {
			loading := map[string]any{}
			for k, v := range title {
				loading[k] = v
			}
			loading["collections"] = []any{}
			title = loading
		}
		reply(map[string]any{"title": title})
	case strings.Contains(body.Query, "me {"):
		reply(map[string]any{"me": map[string]any{"id": "u1"}})
	}
}
func newTestRouter(t *testing.T, fake *fakeScryer) (*Router, *pluginv1.RouterConnection) {
	t.Helper()
	fake.t = t
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	r := New()
	r.now = func() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) }
	conn := &pluginv1.RouterConnection{Id: "c1", BaseUrl: srv.URL + "/", ApiKey: "ska_good"}
	withConfig(t, conn, nil)
	return r, conn
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

// withConfig sets the connection's settings; a series request needs a TMDB key,
// so one is added unless the test sets it.
func withConfig(t *testing.T, conn *pluginv1.RouterConnection, cfg map[string]any) {
	t.Helper()
	if _, ok := cfg[configTMDBKey]; !ok {
		merged := map[string]any{configTMDBKey: "eyJtest"}
		for k, v := range cfg {
			merged[k] = v
		}
		cfg = merged
	}
	st, err := structpb.NewStruct(cfg)
	if err != nil {
		t.Fatal(err)
	}
	conn.Config = st
}

func animeMovie() *pluginv1.RequestDescriptor {
	return &pluginv1.RequestDescriptor{MediaType: "movie", Title: "Your Name", IsAnime: true, ExternalIds: map[string]string{"tmdb": "372058"}}
}

func fulfill(t *testing.T, r *Router, conn *pluginv1.RouterConnection, d *pluginv1.RequestDescriptor) (*pluginv1.FulfillResponse, error) {
	t.Helper()
	return r.Fulfill(context.Background(), &pluginv1.FulfillRequest{
		Request:     d,
		Qualities:   []*pluginv1.RequestedQuality{{Id: "1080p"}},
		Connections: []*pluginv1.RouterConnection{conn},
	})
}

func TestAnimeMovieGoesToTheChosenLibrary(t *testing.T) {
	fake := &fakeScryer{libs: []map[string]any{{"id": "lib-movies", "name": "Movies"}, {"id": "lib-anime", "name": "Anime Movies"}}}
	r, conn := newTestRouter(t, fake)
	withConfig(t, conn, map[string]any{configAnimeMoviesLibrary: "lib-anime"})
	if _, err := fulfill(t, r, conn, animeMovie()); err != nil {
		t.Fatal(err)
	}
	if fake.added["facet"] != "MOVIE" || fake.added["libraryId"] != "lib-anime" {
		t.Fatalf("addTitle input = %v", fake.added)
	}
}

func TestLibraryOnlyAppliesToAnimeMovies(t *testing.T) {
	cfg := map[string]any{configAnimeMoviesLibrary: "lib-anime"}
	for name, d := range map[string]*pluginv1.RequestDescriptor{
		"plain movie":  {MediaType: "movie", Title: "Alien", ExternalIds: map[string]string{"tmdb": "348"}},
		"anime series": {MediaType: "series", Title: "Sailor Moon", IsAnime: true},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeScryer{libs: []map[string]any{{"id": "lib-anime", "name": "Anime Movies"}}}
			r, conn := newTestRouter(t, fake)
			withConfig(t, conn, cfg)
			if _, err := fulfill(t, r, conn, d); err != nil {
				t.Fatal(err)
			}
			if _, ok := fake.added["libraryId"]; ok {
				t.Fatalf("libraryId sent for %s: %v", name, fake.added)
			}
		})
	}
}

func TestAnimeMovieWithoutConfigUsesDefaultLibrary(t *testing.T) {
	fake := &fakeScryer{}
	r, conn := newTestRouter(t, fake)
	if _, err := fulfill(t, r, conn, animeMovie()); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.added["libraryId"]; ok {
		t.Fatalf("libraryId sent without config: %v", fake.added)
	}
}

// A deleted library, or a Scryer that refuses to list libraries, must not stop
// the request: the title goes to the default movie library.
func TestAnimeMovieFallsBackToTheDefaultLibrary(t *testing.T) {
	for name, fake := range map[string]*fakeScryer{
		"library gone":      {libs: []map[string]any{{"id": "lib-movies", "name": "Movies"}}},
		"libraries refused": {libsErr: "permission denied"},
	} {
		t.Run(name, func(t *testing.T) {
			r, conn := newTestRouter(t, fake)
			withConfig(t, conn, map[string]any{configAnimeMoviesLibrary: "lib-anime"})
			resp, err := fulfill(t, r, conn, animeMovie())
			if err != nil {
				t.Fatal(err)
			}
			got := resp.GetTargets()
			if len(got) != 1 || got[0].GetStatus() != statusQueued || !strings.Contains(got[0].GetMessage(), "default movie library") {
				t.Fatalf("targets = %v", got)
			}
			if _, ok := fake.added["libraryId"]; ok {
				t.Fatalf("libraryId sent for a library that could not be used: %v", fake.added)
			}
		})
	}
}

func TestListConfigOptionsOffersMovieLibraries(t *testing.T) {
	fake := &fakeScryer{libs: []map[string]any{{"id": "c45151a6", "name": "Anime Movies"}, {"id": "movie_default_library", "name": "Movies"}}}
	r, conn := newTestRouter(t, fake)
	resp, err := r.ListConfigOptions(context.Background(), &pluginv1.ListConfigOptionsRequest{Connection: conn})
	if err != nil {
		t.Fatal(err)
	}
	opts := resp.GetOptionsByField()[configAnimeMoviesLibrary].GetOptions()
	// Silo fills an empty dynamic choice with the first option, so the default
	// library comes first, named as such, and is not listed twice.
	if len(opts) != 2 || opts[0].GetValue() != "movie_default_library" || opts[0].GetLabel() != "Movies (Scryer's default movie library)" || opts[1].GetValue() != "c45151a6" || opts[1].GetLabel() != "Anime Movies" {
		t.Fatalf("options = %v", opts)
	}
	// Only libraries the key can add titles to are offered.
	if !strings.Contains(fake.libsQuery, "MANAGE_TITLES") {
		t.Fatalf("libraries query = %q", fake.libsQuery)
	}
}

// When the key cannot list the default library, a stand-in with an empty value
// comes first, so the form does not fill the field with another library.
func TestListConfigOptionsKeepsAStandInWhenTheDefaultLibraryIsNotListed(t *testing.T) {
	r, conn := newTestRouter(t, &fakeScryer{libs: []map[string]any{{"id": "c45151a6", "name": "Anime Movies"}}})
	resp, err := r.ListConfigOptions(context.Background(), &pluginv1.ListConfigOptionsRequest{Connection: conn})
	if err != nil {
		t.Fatal(err)
	}
	opts := resp.GetOptionsByField()[configAnimeMoviesLibrary].GetOptions()
	if len(opts) != 2 || opts[0].GetValue() != "" || opts[0].GetLabel() != "Scryer's default movie library" || opts[1].GetValue() != "c45151a6" {
		t.Fatalf("options = %v", opts)
	}
}

func TestListConfigOptionsIsEmptyWhenLibrariesCannotBeRead(t *testing.T) {
	r, conn := newTestRouter(t, &fakeScryer{libsErr: "permission denied"})
	resp, err := r.ListConfigOptions(context.Background(), &pluginv1.ListConfigOptionsRequest{Connection: conn})
	if err != nil || len(resp.GetOptionsByField()) != 0 {
		t.Fatalf("resp = %v, err = %v", resp, err)
	}
}

// A series or anime added whole always says what to monitor: Scryer would
// monitor everything otherwise. A season request sends no options at all.
func TestWholeSeriesAndAnimeSendMissingAndFutureEpisodes(t *testing.T) {
	for name, d := range map[string]*pluginv1.RequestDescriptor{
		"series": {MediaType: "series", Title: "24", ExternalIds: map[string]string{"tvdb": "76290"}},
		"anime":  {MediaType: "series", Title: "Barakamon", IsAnime: true, ExternalIds: map[string]string{"tvdb": "1"}},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeScryer{}
			r, conn := newTestRouter(t, fake)
			if _, err := fulfill(t, r, conn, d); err != nil {
				t.Fatal(err)
			}
			if opts, _ := fake.added["options"].(map[string]any); opts["monitorType"] != "MISSING_AND_FUTURE_EPISODES" {
				t.Fatalf("options = %v", fake.added["options"])
			}
		})
	}
}

// Whether Scryer accepts options.monitorType on a movie is not verified, so a
// movie keeps sending only monitored: true.
func TestMoviesSendNoMonitorOptions(t *testing.T) {
	fake := &fakeScryer{}
	r, conn := newTestRouter(t, fake)
	if _, err := r.Fulfill(context.Background(), &pluginv1.FulfillRequest{
		Request:     &pluginv1.RequestDescriptor{MediaType: "movie", Title: "Alien", ExternalIds: map[string]string{"tmdb": "348"}},
		Qualities:   []*pluginv1.RequestedQuality{{Id: "1080p"}},
		Connections: []*pluginv1.RouterConnection{conn},
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.added["options"]; ok || fake.added["monitored"] != true {
		t.Fatalf("movie input = %v", fake.added)
	}
}

func TestAnimeMovieLibraryLookupRetriesWhenScryerIsUnreachable(t *testing.T) {
	r, conn := newTestRouter(t, &fakeScryer{})
	withConfig(t, conn, map[string]any{configAnimeMoviesLibrary: "Anime Movies"})
	conn.BaseUrl = "http://127.0.0.1:1/"
	if _, err := fulfill(t, r, conn, animeMovie()); err == nil {
		t.Fatal("Fulfill succeeded with Scryer unreachable")
	}
}

// season builds a season of n episodes, the first owned of them with a file and
// the first on of them monitored, ids <id>e1, <id>e2...; first is the air date of
// every episode.
func season(id, kind, index string, flag bool, n, owned, on int, first string) map[string]any {
	eps := []any{}
	for i := 1; i <= n; i++ {
		files := []any{}
		if i <= owned {
			files = append(files, map[string]any{"id": "f"})
		}
		eps = append(eps, map[string]any{"id": fmt.Sprintf("%se%d", id, i), "monitored": i <= on, "mediaFiles": files, "airDate": first})
	}
	return map[string]any{"id": id, "collectionType": kind, "collectionIndex": index, "monitored": flag,
		"episodesOwned": owned, "episodesMonitored": on, "episodesTotal": n, "episodes": eps}
}

func seasonTitle(collections ...map[string]any) map[string]any {
	list := []any{}
	for _, c := range collections {
		list = append(list, c)
	}
	return map[string]any{"id": "t1", "monitored": true, "mediaFiles": []any{map[string]any{"id": "f"}}, "downloadQueueItems": []any{}, "collections": list, "metadataFetchedAt": "2026-11-19T00:00:00Z"}
}

func movieTitle(monitored, file bool) map[string]any {
	files := []any{}
	if file {
		files = append(files, map[string]any{"id": "f"})
	}
	return map[string]any{"id": "t1", "monitored": monitored, "mediaFiles": files, "downloadQueueItems": []any{}}
}

func seasonRequest(seasons ...int32) *pluginv1.RequestDescriptor {
	return &pluginv1.RequestDescriptor{MediaType: "series", Title: "Show", Seasons: seasons, ExternalIds: map[string]string{"tvdb": "259675"}}
}

func calls(list []string) string { return strings.Join(list, ",") }

func tagsOf(added map[string]any) []string {
	var out []string
	for _, t := range added["tags"].([]any) {
		out = append(out, t.(string))
	}
	return out
}

func targetMessage(t *testing.T, resp *pluginv1.FulfillResponse) string {
	t.Helper()
	if len(resp.GetTargets()) != 1 {
		t.Fatalf("targets = %v", resp.GetTargets())
	}
	return resp.GetTargets()[0].GetMessage()
}

// A request ends in a download unless the content already exists: a new movie
// is added monitored, one Scryer has with a file is left alone, and one without a
// file is switched on if it is not monitored.
func TestMovieRequests(t *testing.T) {
	d := &pluginv1.RequestDescriptor{MediaType: "movie", Title: "Alien", RequesterUsername: "ana", ExternalIds: map[string]string{"tmdb": "348"}}
	for name, tc := range map[string]struct {
		reused    bool
		title     map[string]any
		searches  int
		monitored string
		tagged    string
		message   string
	}{
		"new":                               {false, nil, 1, "", "", "Added to Scryer"},
		"existing with a file":              {true, movieTitle(false, true), 0, "", "", "Already in Scryer; nothing to change."},
		"existing monitored without a file": {true, movieTitle(true, false), 1, "", "", "Already in Scryer; searching"},
		"existing unmonitored, no file":     {true, movieTitle(false, false), 1, "t1", "silo-request,silo-user-ana", "Monitored in Scryer"},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeScryer{reused: tc.reused, title: tc.title}
			r, conn := newTestRouter(t, fake)
			withConfig(t, conn, map[string]any{configTagTitles: true, configTagExistingTitles: true})
			resp, err := fulfill(t, r, conn, d)
			if err != nil {
				t.Fatal(err)
			}
			if fake.searches != tc.searches || calls(fake.titleCalls) != tc.monitored || calls(fake.tagged) != tc.tagged {
				t.Fatalf("searches %d, monitored %q, tagged %q", fake.searches, calls(fake.titleCalls), calls(fake.tagged))
			}
			if got := targetMessage(t, resp); !strings.HasPrefix(got, tc.message) {
				t.Fatalf("message = %q", got)
			}
		})
	}
}

// A new title is added unmonitored and without options (a monitor type chosen
// for whole series must not reach it), then each requested season is switched
// on with all its episodes. What Scryer decided for the specials stays.
func TestRequestedSeasonsOfANewTitle(t *testing.T) {
	fake := &fakeScryer{title: seasonTitle(
		season("sp", "SPECIALS", "0", true, 2, 0, 2, ""),
		season("s1", "SEASON", "1", false, 3, 0, 0, ""),
		season("s2", "SEASON", "2", false, 3, 0, 0, ""),
		season("s3", "SEASON", "3", false, 3, 0, 0, ""),
	)}
	r, conn := newTestRouter(t, fake)
	withConfig(t, conn, map[string]any{configTagTitles: true})
	d := seasonRequest(2, 3)
	d.RequesterUsername = "ana"
	if _, err := fulfill(t, r, conn, d); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.added["options"]; ok || fake.added["monitored"] != false {
		t.Fatalf("addTitle input = %v", fake.added)
	}
	if got := calls(fake.collectionCalls); got != "s2:true,s3:true" || len(fake.episodeCalls) != 0 {
		t.Fatalf("collection calls = %q, episode calls = %v", got, fake.episodeCalls)
	}
	if got := calls(tagsOf(fake.added)); got != "silo-request,silo-user-ana" || len(fake.tagged) != 0 {
		t.Fatalf("tags in the add = %q, tagged afterwards = %v", got, fake.tagged)
	}
	if fake.searches != 1 {
		t.Fatalf("searches = %d", fake.searches)
	}
}

// Scryer loads a new title's seasons a few seconds after addTitle returns.
func TestRequestedSeasonsOfANewTitleWaitForScryerToLoadThem(t *testing.T) {
	fake := &fakeScryer{hydrateAfter: 3, title: seasonTitle(season("s1", "SEASON", "1", false, 2, 0, 0, ""))}
	r, conn := newTestRouter(t, fake)
	resp, err := fulfill(t, r, conn, seasonRequest(1))
	if err != nil {
		t.Fatal(err)
	}
	if got := calls(fake.collectionCalls); got != "s1:true" || resp.GetTargets()[0].GetStatus() != statusQueued {
		t.Fatalf("collection calls = %q, targets = %v", got, resp.GetTargets())
	}
}

// A season Scryer switched on with none of its episodes monitored (a "future
// episodes" policy on a finished season) downloads nothing until its episodes
// are on too.
func TestNewTitleSeasonSwitchedOnWithoutMonitoredEpisodesIsMonitored(t *testing.T) {
	fake := &fakeScryer{title: seasonTitle(
		season("s1", "SEASON", "1", true, 12, 0, 0, ""),
		season("s2", "SEASON", "2", true, 12, 0, 12, ""),
	)}
	r, conn := newTestRouter(t, fake)
	if _, err := fulfill(t, r, conn, seasonRequest(1, 2)); err != nil {
		t.Fatal(err)
	}
	if got := calls(fake.collectionCalls); got != "s1:true" {
		t.Fatalf("collection calls = %q, want only s1", got)
	}
}

// In a title Scryer already has, only the episodes of the requested seasons that
// have no file and are not monitored are switched on, one by one. Specials, the
// other seasons, files and monitored episodes are left as they are.
func TestRequestedSeasonsOfAnExistingTitleSwitchOnOnlyWhatIsMissing(t *testing.T) {
	fake := &fakeScryer{reused: true, title: seasonTitle(
		season("sp", "SPECIALS", "0", false, 2, 0, 0, ""),
		season("s1", "SEASON", "1", true, 4, 2, 0, ""), // e1, e2 have files, e3 and e4 are missing
		season("s2", "SEASON", "2", true, 3, 3, 0, ""), // complete
		season("s3", "SEASON", "3", false, 3, 0, 0, ""),
	)}
	r, conn := newTestRouter(t, fake)
	withConfig(t, conn, map[string]any{configTagTitles: true, configTagExistingTitles: true, configSeriesSpecials: true})
	d := seasonRequest(1, 2)
	d.RequesterUsername = "ana"
	resp, err := fulfill(t, r, conn, d)
	if err != nil {
		t.Fatal(err)
	}
	if got := calls(fake.episodeCalls); got != "s1e3,s1e4" || len(fake.collectionCalls) != 0 {
		t.Fatalf("episode calls = %q, collection calls = %v", got, fake.collectionCalls)
	}
	if calls(fake.tagged) != "silo-request,silo-user-ana" || fake.searches != 1 || !strings.HasPrefix(targetMessage(t, resp), "Monitored in Scryer") {
		t.Fatalf("tagged %v, searches %d, message %q", fake.tagged, fake.searches, targetMessage(t, resp))
	}
}

// Nothing to switch on: no change, no tag, no search.
func TestRequestedSeasonsAlreadyOwnedOrMonitoredChangeNothing(t *testing.T) {
	fake := &fakeScryer{reused: true, title: seasonTitle(
		season("s1", "SEASON", "1", true, 4, 2, 0, ""),
		season("s2", "SEASON", "2", true, 3, 3, 0, ""),
	)}
	fake.title["collections"].([]any)[0].(map[string]any)["episodes"] = []any{
		map[string]any{"id": "a", "monitored": false, "mediaFiles": []any{map[string]any{"id": "f"}}},
		map[string]any{"id": "b", "monitored": true, "mediaFiles": []any{}},
	}
	r, conn := newTestRouter(t, fake)
	withConfig(t, conn, map[string]any{configTagTitles: true})
	resp, err := fulfill(t, r, conn, seasonRequest(1, 2))
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.episodeCalls)+len(fake.collectionCalls)+len(fake.tagged)+fake.searches != 0 {
		t.Fatalf("episodes %v, collections %v, tagged %v, searches %d", fake.episodeCalls, fake.collectionCalls, fake.tagged, fake.searches)
	}
	if got := targetMessage(t, resp); got != "Already in Scryer; nothing to change." {
		t.Fatalf("message = %q", got)
	}
}

// Seasons Scryer does not have (an upcoming one, say) are left out and reported;
// the others are requested all the same. With none of them in Scryer the target
// fails and nothing is changed.
func TestRequestedSeasonsMissingInScryer(t *testing.T) {
	for name, reused := range map[string]bool{"new title": false, "existing title": true} {
		t.Run(name+", some missing", func(t *testing.T) {
			fake := &fakeScryer{reused: reused, title: seasonTitle(season("s1", "SEASON", "1", false, 2, 0, 0, ""))}
			r, conn := newTestRouter(t, fake)
			resp, err := fulfill(t, r, conn, seasonRequest(1, 5))
			if err != nil {
				t.Fatal(err)
			}
			got := resp.GetTargets()
			if len(got) != 1 || got[0].GetStatus() != statusQueued || !strings.Contains(got[0].GetMessage(), "Left out, not in Scryer: season 5") {
				t.Fatalf("targets = %v", got)
			}
			if reused {
				if calls(fake.episodeCalls) != "s1e1,s1e2" {
					t.Fatalf("episode calls = %v", fake.episodeCalls)
				}
			} else if calls(fake.collectionCalls) != "s1:true" {
				t.Fatalf("collection calls = %v", fake.collectionCalls)
			}
		})
		t.Run(name+", all missing", func(t *testing.T) {
			fake := &fakeScryer{reused: reused, title: seasonTitle(season("s1", "SEASON", "1", false, 2, 0, 0, ""))}
			r, conn := newTestRouter(t, fake)
			resp, err := fulfill(t, r, conn, seasonRequest(4, 5))
			if err != nil {
				t.Fatal(err)
			}
			got := resp.GetTargets()
			if len(got) != 1 || got[0].GetStatus() != statusFailed || !strings.Contains(got[0].GetMessage(), "seasons 4, 5") {
				t.Fatalf("targets = %v", got)
			}
			if len(fake.collectionCalls)+len(fake.episodeCalls) != 0 {
				t.Fatalf("changed: collections %v, episodes %v", fake.collectionCalls, fake.episodeCalls)
			}
		})
	}
}

// A movie does not need a TMDB key.
func TestMovieDoesNotNeedATMDBKey(t *testing.T) {
	fake := &fakeScryer{}
	r, conn := newTestRouter(t, fake)
	withConfig(t, conn, map[string]any{configTMDBKey: ""})
	if _, err := fulfill(t, r, conn, &pluginv1.RequestDescriptor{MediaType: "movie", Title: "Alien"}); err != nil || fake.added == nil {
		t.Fatalf("movie: err = %v, added = %v", err, fake.added)
	}
}

// A key TMDB refuses fails the target; an unreachable TMDB is retried by Silo.
// Neither leaves anything in Scryer, because TMDB is read before the title is added.
func TestTMDBFailuresLeaveScryerUntouched(t *testing.T) {
	d := seasonRequest(1)
	d.ExternalIds["tmdb"] = "123249"
	t.Run("key refused", func(t *testing.T) {
		fake := &fakeScryer{title: seasonTitle(season("s1", "SEASON", "1", false, 3, 0, 0, ""))}
		r, conn := newTestRouter(t, fake)
		r.tmdbBase = fakeTMDB(t, http.StatusUnauthorized, nil).URL
		// A refused key is a setup problem too: an error, so Silo retries.
		if _, err := fulfill(t, r, conn, d); !errors.Is(err, tmdb.ErrKeyRefused) || fake.added != nil {
			t.Fatalf("err = %v, added = %v", err, fake.added)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		fake := &fakeScryer{title: seasonTitle(season("s1", "SEASON", "1", false, 3, 0, 0, ""))}
		r, conn := newTestRouter(t, fake)
		r.tmdbBase = "http://127.0.0.1:1"
		if _, err := fulfill(t, r, conn, d); err == nil || fake.added != nil {
			t.Fatalf("err = %v, added = %v", err, fake.added)
		}
	})
}

// A series that has not aired yet has no seasons to name: it is requested whole,
// with the monitor type the admin chose. One Scryer already has is looked up first
// and left alone, since the options of an add would replace its settings.
func TestWholeSeriesRequest(t *testing.T) {
	d := &pluginv1.RequestDescriptor{MediaType: "series", Title: "Show", ExternalIds: map[string]string{"tmdb": "100", "tvdb": "1"}}
	t.Run("new title", func(t *testing.T) {
		fake := &fakeScryer{titles: []map[string]any{{"id": "x", "externalIds": []map[string]string{{"source": "tmdb", "value": "999"}}}}}
		r, conn := newTestRouter(t, fake)
		if _, err := fulfill(t, r, conn, d); err != nil {
			t.Fatal(err)
		}
		opts, _ := fake.added["options"].(map[string]any)
		if fake.added["monitored"] != true || opts["monitorType"] != "MISSING_AND_FUTURE_EPISODES" || fake.searches != 1 {
			t.Fatalf("added = %v, searches = %d", fake.added, fake.searches)
		}
	})
	t.Run("title Scryer already has", func(t *testing.T) {
		fake := &fakeScryer{titles: []map[string]any{{"id": "t9", "externalIds": []map[string]string{{"source": "tmdb", "value": "100"}}}}}
		r, conn := newTestRouter(t, fake)
		withConfig(t, conn, map[string]any{configTagTitles: true, configSeriesSpecials: true})
		resp, err := fulfill(t, r, conn, d)
		if err != nil {
			t.Fatal(err)
		}
		got := resp.GetTargets()
		if fake.added != nil || len(fake.tagged) != 0 || fake.searches != 0 || len(got) != 1 || got[0].GetExternalId() != "t9" || got[0].GetStatus() != statusQueued {
			t.Fatalf("added = %v, tagged = %v, searches = %d, targets = %v", fake.added, fake.tagged, fake.searches, got)
		}
	})
}

// The "Monitor specials of new series" switch monitors the specials of a series the
// plugin has just added, by seasons or whole. Anime follow Scryer's own setting
// and a title Scryer already had keeps its specials.
func TestMonitorSpecialsOfNewSeries(t *testing.T) {
	on := map[string]any{configSeriesSpecials: true}
	for name, tc := range map[string]struct {
		reused  bool
		anime   bool
		seasons []int32
		cfg     map[string]any
		sp      map[string]any
		want    string
	}{
		"new series, seasons, on":      {false, false, []int32{1}, on, season("sp", "SPECIALS", "0", false, 4, 0, 0, ""), "s1:true,sp:true"},
		"new series, seasons, off":     {false, false, []int32{1}, nil, season("sp", "SPECIALS", "0", false, 4, 0, 0, ""), "s1:true"},
		"new series, whole, on":        {false, false, nil, on, season("sp", "SPECIALS", "0", false, 4, 0, 0, ""), "sp:true"},
		"new series, whole, off":       {false, false, nil, nil, season("sp", "SPECIALS", "0", false, 4, 0, 0, ""), ""},
		"new series without specials":  {false, false, nil, on, season("sp", "SPECIALS", "0", false, 0, 0, 0, ""), ""},
		"new anime, seasons, on":       {false, true, []int32{1}, on, season("sp", "SPECIALS", "0", false, 4, 0, 0, ""), "s1:true"},
		"new anime, whole, on":         {false, true, nil, on, season("sp", "SPECIALS", "0", false, 4, 0, 0, ""), ""},
		"existing series, seasons, on": {true, false, []int32{1}, on, season("sp", "SPECIALS", "0", false, 4, 0, 0, ""), ""},
	} {
		t.Run(name, func(t *testing.T) {
			// season 1 is already monitored with every episode in an existing title
			s1 := season("s1", "SEASON", "1", false, 3, 0, 0, "")
			if tc.reused {
				s1 = season("s1", "SEASON", "1", true, 3, 0, 3, "")
			}
			fake := &fakeScryer{reused: tc.reused, title: seasonTitle(tc.sp, s1)}
			r, conn := newTestRouter(t, fake)
			if tc.cfg != nil {
				withConfig(t, conn, tc.cfg)
			}
			d := seasonRequest(tc.seasons...)
			d.IsAnime = tc.anime
			if tc.seasons == nil {
				d.ExternalIds["tmdb"] = "100"
			}
			if _, err := fulfill(t, r, conn, d); err != nil {
				t.Fatal(err)
			}
			if got := calls(fake.collectionCalls); got != tc.want {
				t.Fatalf("collection calls = %q, want %q", got, tc.want)
			}
		})
	}
}

// fakeTMDB answers /3/tv/<id> with the status and the seasons given, as a
// [number, air date, episodes] list.
func fakeTMDB(t *testing.T, status int, seasons [][3]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		list := []map[string]any{}
		for _, s := range seasons {
			list = append(list, map[string]any{"season_number": s[0], "air_date": s[1], "episode_count": s[2]})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"seasons": list})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TMDB lists one season of 24 episodes where Scryer has two of 12: the request
// for season 1 reaches both.
func TestSeasonsAreMatchedByAirDate(t *testing.T) {
	for name, reused := range map[string]bool{"new title": false, "existing title": true} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeScryer{reused: reused, title: seasonTitle(
				season("c1", "SEASON", "1", false, 2, 0, 0, "2022-01-09"),
				season("c2", "SEASON", "2", false, 2, 0, 0, "2025-01-06"),
			)}
			r, conn := newTestRouter(t, fake)
			r.tmdbBase = fakeTMDB(t, http.StatusOK, [][3]any{{1, "2022-01-09", 4}}).URL
			d := seasonRequest(1)
			d.ExternalIds["tmdb"] = "123249"
			if _, err := fulfill(t, r, conn, d); err != nil {
				t.Fatal(err)
			}
			if reused {
				if got := calls(fake.episodeCalls); got != "c1e1,c1e2,c2e1,c2e2" {
					t.Fatalf("episode calls = %q", got)
				}
			} else if got := calls(fake.collectionCalls); got != "c1:true,c2:true" {
				t.Fatalf("collection calls = %q", got)
			}
		})
	}
}

// The request is complete when every matched Scryer season has files.
func TestCheckStatusUsesTheMatchedSeasons(t *testing.T) {
	for name, tc := range map[string]struct {
		title map[string]any
		want  string
	}{
		"only one of the two seasons has files": {seasonTitle(season("c1", "SEASON", "1", true, 2, 2, 2, "2022-01-09"), season("c2", "SEASON", "2", true, 2, 0, 2, "2025-01-06")), statusQueued},
		"one has only part of its episodes":     {seasonTitle(season("c1", "SEASON", "1", true, 2, 2, 2, "2022-01-09"), season("c2", "SEASON", "2", true, 2, 1, 2, "2025-01-06")), statusQueued},
		"both have all their episodes":          {seasonTitle(season("c1", "SEASON", "1", true, 2, 2, 2, "2022-01-09"), season("c2", "SEASON", "2", true, 2, 2, 2, "2025-01-06")), statusCompleted},
	} {
		t.Run(name, func(t *testing.T) {
			r, conn := newTestRouter(t, &fakeScryer{title: tc.title})
			r.tmdbBase = fakeTMDB(t, http.StatusOK, [][3]any{{1, "2022-01-09", 4}}).URL
			d := seasonRequest(1)
			d.ExternalIds["tmdb"] = "123249"
			resp, err := r.CheckStatus(context.Background(), &pluginv1.CheckStatusRequest{
				Request:     d,
				Targets:     []*pluginv1.TargetRef{{Quality: "1080p", ConnectionId: "c1", ExternalId: "t1"}},
				Connections: []*pluginv1.RouterConnection{conn},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := resp.GetStatuses()[0].GetStatus(); got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

// A request that names seasons completes when each has a file, not when the
// title has any.
func TestCheckStatusCompletesOnlyWhenTheRequestedSeasonsHaveFiles(t *testing.T) {
	for name, tc := range map[string]struct {
		seasons []int32
		want    string
	}{
		"season 1 has all its episodes":      {[]int32{1}, statusCompleted},
		"season 2 has none":                  {[]int32{2}, statusQueued},
		"one of two requested has none":      {[]int32{1, 2}, statusQueued},
		"no seasons: any file of the title":  {nil, statusCompleted},
		"a season Scryer lacks is left out":  {[]int32{1, 5}, statusCompleted},
		"none of the requested is in Scryer": {[]int32{5}, statusQueued},
	} {
		t.Run(name, func(t *testing.T) {
			title := seasonTitle(season("s1", "SEASON", "1", true, 2, 2, 2, ""), season("s2", "SEASON", "2", true, 2, 0, 2, ""))
			r, conn := newTestRouter(t, &fakeScryer{title: title})
			resp, err := r.CheckStatus(context.Background(), &pluginv1.CheckStatusRequest{
				Request:     seasonRequest(tc.seasons...),
				Targets:     []*pluginv1.TargetRef{{Quality: "1080p", ConnectionId: "c1", ExternalId: "t1"}},
				Connections: []*pluginv1.RouterConnection{conn},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := resp.GetStatuses()[0].GetStatus(); got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRequestTagsAreCreatedAndSentWithANewTitle(t *testing.T) {
	fake := &fakeScryer{tagDefs: []string{"silo-request"}}
	r, conn := newTestRouter(t, fake)
	withConfig(t, conn, map[string]any{configTagTitles: true})
	d := &pluginv1.RequestDescriptor{MediaType: "movie", Title: "Alien", RequesterUsername: "Ana Maria", ExternalIds: map[string]string{"tmdb": "348"}}
	if _, err := fulfill(t, r, conn, d); err != nil {
		t.Fatal(err)
	}
	if got := calls(tagsOf(fake.added)); got != "silo-request,silo-user-ana-maria" {
		t.Fatalf("tags = %s", got)
	}
	if len(fake.tagDefs) != 2 || fake.tagDefs[1] != "silo-user-ana-maria" {
		t.Fatalf("registry = %v, want the missing user tag created and silo-request left alone", fake.tagDefs)
	}
	if len(fake.tagged) != 0 {
		t.Fatalf("a new title was tagged again: %v", fake.tagged)
	}
}

func TestRequestWithoutUsernameGetsOnlyTheCommonTag(t *testing.T) {
	fake := &fakeScryer{}
	r, conn := newTestRouter(t, fake)
	withConfig(t, conn, map[string]any{configTagTitles: true})
	if _, err := fulfill(t, r, conn, &pluginv1.RequestDescriptor{MediaType: "movie", Title: "Alien"}); err != nil {
		t.Fatal(err)
	}
	if got := calls(tagsOf(fake.added)); got != "silo-request" {
		t.Fatalf("tags = %s", got)
	}
}

// A refused tag creation must not lose the request.
func TestTitleIsAddedWithoutTagsWhenTheyCannotBeCreated(t *testing.T) {
	fake := &fakeScryer{tagCreateErr: "permission denied"}
	r, conn := newTestRouter(t, fake)
	withConfig(t, conn, map[string]any{configTagTitles: true})
	resp, err := fulfill(t, r, conn, &pluginv1.RequestDescriptor{MediaType: "movie", Title: "Alien", RequesterUsername: "ana"})
	if err != nil {
		t.Fatal(err)
	}
	got := resp.GetTargets()
	if len(got) != 1 || got[0].GetStatus() != statusQueued || !strings.Contains(got[0].GetMessage(), "Not tagged") {
		t.Fatalf("targets = %v", got)
	}
	if len(tagsOf(fake.added)) != 0 {
		t.Fatalf("tags sent although they could not be created: %v", fake.added["tags"])
	}
}

// Like every setting the plugin adds, tagging does nothing until it is chosen.
func TestTaggingIsOffByDefault(t *testing.T) {
	fake := &fakeScryer{reused: true, title: movieTitle(false, false)}
	r, conn := newTestRouter(t, fake)
	if _, err := fulfill(t, r, conn, &pluginv1.RequestDescriptor{MediaType: "movie", Title: "Alien", RequesterUsername: "ana"}); err != nil {
		t.Fatal(err)
	}
	if len(tagsOf(fake.added)) != 0 || len(fake.tagDefs) != 0 || len(fake.tagged) != 0 {
		t.Fatalf("tagging happened while off: tags %v, registry %v, tagged %v", fake.added["tags"], fake.tagDefs, fake.tagged)
	}
}

// One tag that cannot be defined must not cost the title the others.
func TestTagsThatExistAreKeptWhenAnotherCannotBeCreated(t *testing.T) {
	fake := &fakeScryer{tagDefs: []string{"silo-request"}, tagCreateErr: "permission denied"}
	r, conn := newTestRouter(t, fake)
	withConfig(t, conn, map[string]any{configTagTitles: true})
	resp, err := fulfill(t, r, conn, &pluginv1.RequestDescriptor{MediaType: "movie", Title: "Alien", RequesterUsername: "ana"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tagsOf(fake.added), ","); got != "silo-request" {
		t.Fatalf("tags = %q, want silo-request kept", got)
	}
	if msg := resp.GetTargets()[0].GetMessage(); !strings.Contains(msg, "silo-user-ana") {
		t.Fatalf("message does not name the dropped tag: %q", msg)
	}
}

// Two requests from a new user define the same tag at once; the one that
// loses the race still tags its title.
func TestTagDefinedByAConcurrentRequestIsUsed(t *testing.T) {
	fake := &fakeScryer{tagDefs: []string{"silo-request"}, tagCreateRace: true}
	r, conn := newTestRouter(t, fake)
	withConfig(t, conn, map[string]any{configTagTitles: true})
	if _, err := fulfill(t, r, conn, &pluginv1.RequestDescriptor{MediaType: "movie", Title: "Alien", RequesterUsername: "ana"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tagsOf(fake.added), ","); got != "silo-request,silo-user-ana" {
		t.Fatalf("tags = %q", got)
	}
}

// Silo usernames are free text and can be email addresses: the tag keeps
// to letters, digits and dashes, never carries an address, and falls back
// to the account id when nothing usable is left.
func TestUserTagIsSafeForAnyUsername(t *testing.T) {
	for name, want := range map[string]string{
		"Ana Maria":             "silo-user-ana-maria",
		"ana.maria@example.com": "silo-user-ana-maria",
		"Zoë O'Brien":           "silo-user-zo-o-brien",
		"  --Bob__--  ":         "silo-user-bob",
		"!!!":                   "silo-user-7",
		"":                      "",
	} {
		d := &pluginv1.RequestDescriptor{RequesterUsername: name, RequesterUserId: 7}
		if name == "" {
			d.RequesterUserId = 0
		}
		got := userTag(d)
		if got != want {
			t.Errorf("userTag(%q) = %q, want %q", name, got, want)
		}
	}
	long := userTag(&pluginv1.RequestDescriptor{RequesterUsername: strings.Repeat("a", 100)})
	if len(long) > len("silo-user-")+maxUserTagLength {
		t.Errorf("tag not capped: %d chars", len(long))
	}
}

// Scryer may store a label in another case; it is still the same tag.
func TestExistingTagMatchesWhateverItsCase(t *testing.T) {
	fake := &fakeScryer{tagDefs: []string{"Silo-Request", "SILO-USER-ANA"}, tagCreateErr: "should not be called"}
	r, conn := newTestRouter(t, fake)
	withConfig(t, conn, map[string]any{configTagTitles: true})
	resp, err := fulfill(t, r, conn, &pluginv1.RequestDescriptor{MediaType: "movie", Title: "Alien", RequesterUsername: "ana"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resp.GetTargets()[0].GetMessage(), "Not tagged") || len(tagsOf(fake.added)) != 2 {
		t.Fatalf("existing tags not recognised: %v, %q", fake.added["tags"], resp.GetTargets()[0].GetMessage())
	}
}

// Scryer briefly unreachable while tags are checked: Silo retries the
// request rather than the title being added untagged for good.
func TestUnreachableTagRegistryRetriesTheRequest(t *testing.T) {
	fake := &fakeScryer{tagsDown: true}
	r, conn := newTestRouter(t, fake)
	withConfig(t, conn, map[string]any{configTagTitles: true})
	if _, err := fulfill(t, r, conn, &pluginv1.RequestDescriptor{MediaType: "movie", Title: "Alien", RequesterUsername: "ana"}); err == nil {
		t.Fatal("Fulfill succeeded while the tag registry was unreachable")
	}
	if fake.added != nil {
		t.Fatal("title added untagged while Scryer was unreachable")
	}
}

// Scryer matches existing titles within one library, so a series Silo calls anime
// but Scryer keeps in the Series library (or the reverse) is found there and not
// added a second time.
func TestSeriesIsLookedUpInBothLibrariesBeforeItIsAdded(t *testing.T) {
	lib := []map[string]any{{"id": "t1", "externalIds": []map[string]string{{"source": "tmdb", "value": "100"}}}}
	d := seasonRequest(1)
	d.IsAnime = true
	d.ExternalIds["tmdb"] = "100"
	t.Run("kept in the other library: no second copy", func(t *testing.T) {
		fake := &fakeScryer{titles: lib, titlesIn: "SERIES", title: seasonTitle(season("s1", "SEASON", "1", false, 2, 0, 0, "2022-01-09"))}
		r, conn := newTestRouter(t, fake)
		r.tmdbBase = fakeTMDB(t, http.StatusOK, [][3]any{{1, "2022-01-09", 2}}).URL
		withConfig(t, conn, map[string]any{configTagTitles: true, configTagExistingTitles: true})
		resp, err := fulfill(t, r, conn, d)
		if err != nil {
			t.Fatal(err)
		}
		if fake.added != nil || calls(fake.findFacets) != "ANIME,SERIES" {
			t.Fatalf("added = %v, lookups = %v", fake.added, fake.findFacets)
		}
		if calls(fake.episodeCalls) != "s1e1,s1e2" || !strings.HasPrefix(targetMessage(t, resp), "Monitored in Scryer") || len(fake.tagged) == 0 {
			t.Fatalf("episodes %v, message %q, tagged %v", fake.episodeCalls, targetMessage(t, resp), fake.tagged)
		}
	})
	t.Run("whole series, kept in the other library", func(t *testing.T) {
		fake := &fakeScryer{titles: lib, titlesIn: "SERIES"}
		r, conn := newTestRouter(t, fake)
		w := &pluginv1.RequestDescriptor{MediaType: "series", Title: "Show", IsAnime: true, ExternalIds: map[string]string{"tmdb": "100"}}
		resp, err := fulfill(t, r, conn, w)
		if err != nil || fake.added != nil || targetMessage(t, resp) != "Already in Scryer; nothing to change." {
			t.Fatalf("err = %v, added = %v, message %q", err, fake.added, targetMessage(t, resp))
		}
	})
	t.Run("in neither: added in the facet Silo names", func(t *testing.T) {
		fake := &fakeScryer{title: seasonTitle(season("s1", "SEASON", "1", false, 2, 0, 0, "2022-01-09"))}
		r, conn := newTestRouter(t, fake)
		r.tmdbBase = fakeTMDB(t, http.StatusOK, [][3]any{{1, "2022-01-09", 2}}).URL
		if _, err := fulfill(t, r, conn, d); err != nil {
			t.Fatal(err)
		}
		if fake.added["facet"] != "ANIME" {
			t.Fatalf("added = %v", fake.added)
		}
	})
}

// An upcoming season of a title Scryer already has has no episodes to monitor one
// by one: the season itself is monitored, which also monitors the title, so its
// episodes download once they appear. One already monitored is left alone.
func TestUpcomingSeasonOfAnExistingTitleIsMonitored(t *testing.T) {
	for name, tc := range map[string]struct {
		flag bool
		want string
	}{"flag off": {false, "s2:true"}, "flag on": {true, ""}} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeScryer{reused: true, title: seasonTitle(season("s1", "SEASON", "1", true, 2, 2, 0, ""), season("s2", "SEASON", "2", tc.flag, 0, 0, 0, ""))}
			r, conn := newTestRouter(t, fake)
			withConfig(t, conn, map[string]any{configTagTitles: true, configTagExistingTitles: true})
			resp, err := fulfill(t, r, conn, seasonRequest(2))
			if err != nil {
				t.Fatal(err)
			}
			if got := calls(fake.collectionCalls); got != tc.want || len(fake.episodeCalls) != 0 {
				t.Fatalf("collection calls = %q, episode calls = %v", got, fake.episodeCalls)
			}
			if (tc.want != "") != (len(fake.tagged) > 0) || (tc.want != "") != strings.HasPrefix(targetMessage(t, resp), "Monitored in Scryer") {
				t.Fatalf("tagged %v, message %q", fake.tagged, targetMessage(t, resp))
			}
		})
	}
}

// An anime movie that is already in another movie library (the default one, say)
// is found there and not added to the chosen library a second time.
func TestMovieIsLookedUpInAllMovieLibrariesBeforeItIsAdded(t *testing.T) {
	lib := []map[string]any{{"id": "t1", "externalIds": []map[string]string{{"source": "tmdb", "value": "372058"}}}}
	for name, tc := range map[string]struct {
		title    map[string]any
		searches int
		titles   string
		message  string
	}{
		"with a file":    {movieTitle(false, true), 0, "", "Already in Scryer; nothing to change."},
		"without a file": {movieTitle(false, false), 1, "t1", "Monitored in Scryer"},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeScryer{titles: lib, title: tc.title, libs: []map[string]any{{"id": "anime-movies", "name": "Anime Movies"}}}
			r, conn := newTestRouter(t, fake)
			withConfig(t, conn, map[string]any{configAnimeMoviesLibrary: "anime-movies"})
			resp, err := fulfill(t, r, conn, animeMovie())
			if err != nil {
				t.Fatal(err)
			}
			if fake.added != nil || calls(fake.findFacets) != "MOVIE" || fake.searches != tc.searches || calls(fake.titleCalls) != tc.titles || !strings.HasPrefix(targetMessage(t, resp), tc.message) {
				t.Fatalf("added = %v, lookups = %v, searches %d, monitored %v, message %q", fake.added, fake.findFacets, fake.searches, fake.titleCalls, targetMessage(t, resp))
			}
		})
	}
}

// A new title whose metadata Scryer has loaded (metadataFetchedAt is set, the
// seasons are there) and that still lacks a requested season does not make the
// request wait for it: the season is not coming.
func TestMissingSeasonOfANewTitleDoesNotWait(t *testing.T) {
	title := seasonTitle(season("s1", "SEASON", "1", false, 2, 0, 0, ""))
	title["metadataFetchedAt"] = "2026-11-19T00:00:00Z"
	fake := &fakeScryer{title: title}
	r, conn := newTestRouter(t, fake)
	resp, err := fulfill(t, r, conn, seasonRequest(1, 3))
	if err != nil {
		t.Fatal(err)
	}
	if fake.reads != 1 || !strings.Contains(targetMessage(t, resp), "Left out, not in Scryer: season 3") {
		t.Fatalf("title reads = %d, message %q", fake.reads, targetMessage(t, resp))
	}
}

// Only the API Read Access Token is accepted: it cannot change anything on TMDB,
// the API key can. A key that is not a token is refused by the form.
func TestOnlyTheTMDBReadAccessTokenIsAccepted(t *testing.T) {
	apiKey := "0123456789abcdef0123456789abcdef"
	r, conn := newTestRouter(t, &fakeScryer{})
	withConfig(t, conn, map[string]any{configTMDBKey: apiKey})
	v, err := r.Validate(context.Background(), &pluginv1.ValidateRequest{Connection: conn})
	if err != nil || v.GetFieldErrors()[configTMDBKey] == "" {
		t.Fatalf("validate = %v, %v", v, err)
	}
	withConfig(t, conn, map[string]any{configTMDBKey: "eyJtoken"})
	if v, err := r.Validate(context.Background(), &pluginv1.ValidateRequest{Connection: conn}); err != nil || len(v.GetFieldErrors()) != 0 {
		t.Fatalf("a token must pass: %v, %v", v, err)
	}
}

// The token is optional: an empty one saves (series then use Silo's season
// numbers as they are); an API key is still refused.
func TestFormAcceptsAnEmptyTMDBTokenButNotAnAPIKey(t *testing.T) {
	r, conn := newTestRouter(t, &fakeScryer{})
	withConfig(t, conn, map[string]any{configTMDBKey: ""})
	if v, err := r.Validate(context.Background(), &pluginv1.ValidateRequest{Connection: conn}); err != nil || len(v.GetFieldErrors()) != 0 {
		t.Fatalf("empty: validate = %v, %v", v, err)
	}
	withConfig(t, conn, map[string]any{configTMDBKey: "0123456789abcdef0123456789abcdef"})
	if v, err := r.Validate(context.Background(), &pluginv1.ValidateRequest{Connection: conn}); err != nil || v.GetFieldErrors()[configTMDBKey] == "" {
		t.Fatalf("API key: validate = %v, %v", v, err)
	}
}

// A title TMDB does not know is not a setup problem: it fails the target.
func TestTitleUnknownToTMDBFailsTheTarget(t *testing.T) {
	fake := &fakeScryer{}
	r, conn := newTestRouter(t, fake)
	r.tmdbBase = fakeTMDB(t, http.StatusNotFound, nil).URL
	d := seasonRequest(1)
	d.ExternalIds["tmdb"] = "1"
	resp, err := fulfill(t, r, conn, d)
	if err != nil {
		t.Fatal(err)
	}
	got := resp.GetTargets()
	if len(got) != 1 || got[0].GetStatus() != statusFailed || fake.added != nil {
		t.Fatalf("targets = %v, added = %v", got, fake.added)
	}
}

// If Scryer has not loaded a new series' specials when the wait ends, the request
// is not failed or retried for them: the title is already in place.
func TestSpecialsNotLoadedYetDoNotFailTheRequest(t *testing.T) {
	title := seasonTitle()
	title["metadataFetchedAt"] = nil
	fake := &fakeScryer{title: title, hydrateAfter: 1000}
	r, conn := newTestRouter(t, fake)
	withConfig(t, conn, map[string]any{configSeriesSpecials: true})
	w := &pluginv1.RequestDescriptor{MediaType: "series", Title: "Show", ExternalIds: map[string]string{"tvdb": "1"}}
	resp, err := fulfill(t, r, conn, w)
	if err != nil || len(resp.GetTargets()) != 1 || resp.GetTargets()[0].GetStatus() != statusQueued {
		t.Fatalf("resp = %v, err = %v", resp, err)
	}
}

// A new title whose seasons Scryer has still not loaded when the wait ends (its
// metadata is not fetched) is a matter of time: an error, so Silo retries later.
// The retry finds the title in Scryer and carries on.
func TestNewTitleStillLoadingAfterTheWaitIsRetried(t *testing.T) {
	title := seasonTitle(season("s1", "SEASON", "1", false, 2, 0, 0, ""))
	title["metadataFetchedAt"] = nil
	fake := &fakeScryer{title: title, hydrateAfter: 1000}
	r, conn := newTestRouter(t, fake)
	if _, err := fulfill(t, r, conn, seasonRequest(1)); !errors.Is(err, scryer.ErrNotLoaded) || len(fake.collectionCalls)+len(fake.episodeCalls) != 0 {
		t.Fatalf("err = %v, collections %v, episodes %v", err, fake.collectionCalls, fake.episodeCalls)
	}
	// Later, Scryer has loaded it: the title is now an existing one.
	fake.hydrateAfter = 0
	fake.title["metadataFetchedAt"] = "2026-11-19T00:00:00Z"
	fake.reused = true
	fake.titles = []map[string]any{{"id": "t1", "externalIds": []map[string]string{{"source": "tmdb", "value": "100"}}}}
	d := seasonRequest(1)
	d.ExternalIds["tmdb"] = "100"
	r.tmdbBase = fakeTMDB(t, http.StatusOK, [][3]any{{1, "", 2}}).URL
	if _, err := fulfill(t, r, conn, d); err != nil || calls(fake.episodeCalls) != "s1e1,s1e2" {
		t.Fatalf("retry: err = %v, episodes %v", err, fake.episodeCalls)
	}
}

// Hardening from review.

// With no token the request still goes through, using Silo's season numbers
// as they are, and TMDB is not called at all.
func TestSeriesRequestWithoutATMDBTokenUsesSilosSeasonNumbers(t *testing.T) {
	fake := &fakeScryer{title: seasonTitle(season("s1", "SEASON", "1", false, 2, 0, 0, ""), season("s2", "SEASON", "2", false, 2, 0, 0, ""))}
	r, conn := newTestRouter(t, fake)
	withConfig(t, conn, map[string]any{configTMDBKey: ""})
	r.tmdbBase = fakeTMDB(t, http.StatusUnauthorized, nil).URL // must not be asked
	d := seasonRequest(2)
	d.ExternalIds["tmdb"] = "1973"
	d.IsAnime = true
	resp, err := fulfill(t, r, conn, d)
	if err != nil {
		t.Fatal(err)
	}
	if got := calls(fake.collectionCalls); got != "s2:true" {
		t.Fatalf("collection calls = %q", got)
	}
	if msg := targetMessage(t, resp); !strings.Contains(msg, "TMDB") {
		t.Fatalf("anime without a token should say its seasons were not matched: %q", msg)
	}
}

// Tags can drive Scryer's maintenance rules, so a title Scryer already had is
// tagged only when the admin allows it, even when the request changed it: a
// rule on silo-request must not reach the admin's own series because a user
// asked for its new season.
func TestChangedExistingTitleIsTaggedOnlyWhenAllowed(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg  map[string]any
		want string
	}{
		"not allowed": {map[string]any{configTagTitles: true}, ""},
		"allowed":     {map[string]any{configTagTitles: true, configTagExistingTitles: true}, "silo-request,silo-user-ana"},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeScryer{reused: true, title: seasonTitle(season("s1", "SEASON", "1", true, 2, 0, 0, ""))}
			r, conn := newTestRouter(t, fake)
			withConfig(t, conn, tc.cfg)
			d := seasonRequest(1)
			d.RequesterUsername = "ana"
			if _, err := fulfill(t, r, conn, d); err != nil {
				t.Fatal(err)
			}
			if calls(fake.episodeCalls) != "s1e1,s1e2" || calls(fake.tagged) != tc.want {
				t.Fatalf("episodes %v, tagged %q, want %q", fake.episodeCalls, calls(fake.tagged), tc.want)
			}
		})
	}
}

// The lookup only prevents duplicates: if Scryer refuses it, the request goes
// on as before the lookup existed (Scryer still matches within one library).
func TestRefusedLookupStillAddsTheTitle(t *testing.T) {
	fake := &fakeScryer{findErr: "not allowed"}
	r, conn := newTestRouter(t, fake)
	resp, err := fulfill(t, r, conn, &pluginv1.RequestDescriptor{MediaType: "movie", Title: "Alien", ExternalIds: map[string]string{"tmdb": "348"}})
	if err != nil {
		t.Fatal(err)
	}
	if fake.added == nil || resp.GetTargets()[0].GetStatus() != statusQueued {
		t.Fatalf("added %v, targets %v", fake.added, resp.GetTargets())
	}
}

// Silo gives Fulfill 60 seconds. The waits for a new title's seasons share one
// budget, so a title Scryer never finishes loading ends in ErrNotLoaded (Silo
// retries) well inside it, instead of three waits in a row running past it.
func TestSeasonWaitsShareOneBudget(t *testing.T) {
	defer func(p time.Duration, b time.Duration) { scryer.SeasonPoll, seasonWaitBudget = p, b }(scryer.SeasonPoll, seasonWaitBudget)
	scryer.SeasonPoll = 20 * time.Millisecond // each wait alone: 10 x 20ms
	seasonWaitBudget = 60 * time.Millisecond
	title := seasonTitle(season("s1", "SEASON", "1", false, 2, 0, 0, ""))
	title["metadataFetchedAt"] = nil
	fake := &fakeScryer{title: title, hydrateAfter: 1000}
	r, conn := newTestRouter(t, fake)
	withConfig(t, conn, map[string]any{configSeriesSpecials: true})
	r.tmdbBase = fakeTMDB(t, http.StatusOK, [][3]any{{1, "2022-01-09", 2}}).URL
	d := seasonRequest(1)
	d.ExternalIds["tmdb"] = "100"
	start := time.Now()
	_, err := fulfill(t, r, conn, d)
	if !errors.Is(err, scryer.ErrNotLoaded) {
		t.Fatalf("err = %v, want ErrNotLoaded", err)
	}
	if took := time.Since(start); took > 200*time.Millisecond {
		t.Fatalf("took %v: the waits did not share the budget", took)
	}
}

// seasonOf builds a season from a string, one letter per episode: "o" is owned
// and not monitored (a request leaves it alone), "M" is monitored with its file,
// "m" is monitored and still missing.
func seasonOf(index, episodes string) map[string]any {
	eps := []any{}
	owned, monitored := 0, 0
	for i, ch := range episodes {
		files := []any{}
		if ch == 'o' || ch == 'M' {
			files = append(files, map[string]any{"id": "f"})
			owned++
		}
		if ch == 'M' || ch == 'm' {
			monitored++
		}
		eps = append(eps, map[string]any{"id": fmt.Sprintf("s%se%d", index, i+1), "monitored": ch == 'M' || ch == 'm', "mediaFiles": files})
	}
	return map[string]any{"id": "s" + index, "collectionType": "SEASON", "collectionIndex": index, "monitored": true,
		"episodesOwned": owned, "episodesMonitored": monitored, "episodesTotal": len(episodes), "episodes": eps}
}

// A request leaves the episodes Scryer already owns unmonitored, so "owned" and
// "monitored" in the collection counts are different sets: a season with more
// owned episodes than monitored ones is not done while a monitored one is missing.
func TestSeasonIsDoneWhenEveryMonitoredEpisodeHasAFile(t *testing.T) {
	for name, tc := range map[string]struct {
		episodes string
		want     string
	}{
		"owned ones outnumber the missing monitored ones": {"ooooooommmmm", statusQueued},
		"some of the monitored ones downloaded":           {"oooooMMmmmm", statusQueued},
		"every monitored one downloaded":                  {"oooMMMMM", statusCompleted},
		"nothing owned yet":                               {"mmmm", statusQueued},
		"a monitored episode still to air":                {"MMMm", statusQueued},
	} {
		t.Run(name, func(t *testing.T) {
			r, conn := newTestRouter(t, &fakeScryer{title: seasonTitle(seasonOf("1", tc.episodes))})
			resp, err := r.CheckStatus(context.Background(), &pluginv1.CheckStatusRequest{
				Request:     seasonRequest(1),
				Targets:     []*pluginv1.TargetRef{{Quality: "1080p", ConnectionId: "c1", ExternalId: "t1"}},
				Connections: []*pluginv1.RouterConnection{conn},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := resp.GetStatuses()[0].GetStatus(); got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}
