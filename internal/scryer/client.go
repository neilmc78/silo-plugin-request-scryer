// Package scryer talks to Scryer's GraphQL API with an API key.
package scryer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/neilmc78/silo-plugin-request-scryer/internal/seasonmap"
)

// ErrUnauthorized means Scryer refused the API key.
var ErrUnauthorized = errors.New("scryer refused the API key")

// GraphQLError is an error Scryer reported in a 200 response, such as a
// validation failure. Retrying the same call will not help.
type GraphQLError struct{ Messages []string }

func (e *GraphQLError) Error() string { return "scryer: " + strings.Join(e.Messages, "; ") }

// Client calls one Scryer server.
type Client struct {
	HTTP    *http.Client
	BaseURL string
	APIKey  string
}

// New returns a client for the Scryer at baseURL, such as
// http://scryer:8080 or https://example.org/scryer.
func New(baseURL, apiKey string) *Client {
	return &Client{
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:  strings.TrimSpace(apiKey),
	}
}

// AddTitle is the subset of Scryer's AddTitleInput this plugin sends.
type AddTitle struct {
	Name      string        `json:"name"`
	Facet     string        `json:"facet"`               // MOVIE, SERIES or ANIME
	LibraryID *string       `json:"libraryId,omitempty"` // nil: Scryer picks the facet's default library
	Monitored bool          `json:"monitored"`
	Options   *TitleOptions `json:"options,omitempty"`
	Tags      []string      `json:"tags"`
	Year      *int          `json:"year,omitempty"`
	TmdbID    *int64        `json:"tmdbId,omitempty"`
	TvdbID    *string       `json:"tvdbId,omitempty"`
	ImdbID    *string       `json:"imdbId,omitempty"`
}

// TitleOptions is the subset of Scryer's TitleOptionsInput this plugin sends.
type TitleOptions struct {
	// MonitorType is one of Scryer's MonitorTypeValue names. Only sent for
	// series and anime; whether Scryer accepts it on a movie is not verified.
	MonitorType string `json:"monitorType,omitempty"`
}

// QueueItem is one download Scryer tracks for a title.
type QueueItem struct {
	DisplayState     string `json:"displayState"`
	ProgressPercent  int    `json:"progressPercent"`
	SizeBytes        *int64 `json:"sizeBytes"`
	RemainingSeconds *int   `json:"remainingSeconds"`
}

// Collection is a season (or the specials) of a series in Scryer.
type Collection struct {
	ID            string `json:"id"`
	Type          string `json:"collectionType"`  // SEASON, SPECIALS, MOVIE, ARC
	Index         string `json:"collectionIndex"` // the season number, "0" for specials
	Monitored     bool   `json:"monitored"`
	EpisodesOwned *int   `json:"episodesOwned"`

	// Monitored episodes and all episodes of the season; nil when Scryer has none to count.
	EpisodesMonitored *int `json:"episodesMonitored"`
	EpisodesTotal     *int `json:"episodesTotal"`

	Episodes []Episode `json:"episodes"`
}

// Episode is one episode of a season, with whether Scryer has a file for it.
type Episode struct {
	ID         string     `json:"id"`
	Monitored  bool       `json:"monitored"`
	MediaFiles []struct{} `json:"mediaFiles"`
}

// unmonitoredEpisodes reports whether some episodes of the season are not
// monitored: a season can be switched on with all its episodes off, which is
// how a monitor policy such as future episodes leaves a finished season.
func (c Collection) unmonitoredEpisodes() bool {
	return c.EpisodesMonitored != nil && c.EpisodesTotal != nil && *c.EpisodesMonitored < *c.EpisodesTotal
}

// Title is the state of one title in Scryer.
type Title struct {
	ID          string       `json:"id"`
	Monitored   bool         `json:"monitored"`
	MediaFiles  []struct{}   `json:"mediaFiles"`
	Queue       []QueueItem  `json:"downloadQueueItems"`
	Collections []Collection `json:"collections"`
}

// Me checks the API key; it fails with ErrUnauthorized when Scryer refuses it.
func (c *Client) Me(ctx context.Context) error {
	var out struct {
		Me *struct {
			ID string `json:"id"`
		} `json:"me"`
	}
	if err := c.do(ctx, `query SiloCheck { me { id } }`, nil, &out); err != nil {
		return err
	}
	if out.Me == nil {
		return ErrUnauthorized
	}
	return nil
}

// Library is one Scryer library.
type Library struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Slug  string `json:"slug"`
	Facet string `json:"facet"`
}

// Libraries lists the libraries of a facet (MOVIE, SERIES or ANIME) the API
// key can add titles to. Scryer lists libraries the key can only view unless
// asked for a permission, and adding a title needs ManageTitles.
func (c *Client) Libraries(ctx context.Context, facet string) ([]Library, error) {
	var out struct {
		Libraries []Library `json:"libraries"`
	}
	const q = `query SiloLibraries($facet: MediaFacetValue) { libraries(facet: $facet, permission: MANAGE_TITLES) { id name slug facet } }`
	if err := c.do(ctx, q, map[string]any{"facet": facet}, &out); err != nil {
		return nil, err
	}
	return out.Libraries, nil
}

// ErrLibraryNotFound means no library of the facet matches.
var ErrLibraryNotFound = errors.New("no such library")

// LibraryID checks that a library of the facet with this id exists and returns
// it.
func (c *Client) LibraryID(ctx context.Context, facet, id string) (string, error) {
	libs, err := c.Libraries(ctx, facet)
	if err != nil {
		return "", err
	}
	for _, l := range libs {
		if l.ID == id {
			return l.ID, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrLibraryNotFound, id)
}

// AddedTitle is the result of AddTitle.
type AddedTitle struct {
	ID string
	// Reused is true when Scryer already had the title: it is then not new, so
	// its seasons are already loaded and its other seasons are left alone.
	Reused bool
}

// AddTitle creates the title, or reuses it when Scryer already has it in the
// target library, and returns its ID.
func (c *Client) AddTitle(ctx context.Context, in AddTitle) (AddedTitle, error) {
	var out struct {
		AddTitle struct {
			Title struct {
				ID string `json:"id"`
			} `json:"title"`
			Reused bool `json:"reusedExistingTitle"`
		} `json:"addTitle"`
	}
	const q = `mutation SiloAddTitle($input: AddTitleInput!) { addTitle(input: $input) { title { id } reusedExistingTitle } }`
	if err := c.do(ctx, q, map[string]any{"input": in}, &out); err != nil {
		return AddedTitle{}, err
	}
	if out.AddTitle.Title.ID == "" {
		return AddedTitle{}, errors.New("scryer returned no title id")
	}
	return AddedTitle{ID: out.AddTitle.Title.ID, Reused: out.AddTitle.Reused}, nil
}

// EnsureTags makes sure each label is defined in Scryer's title-tag registry
// and returns the labels to send, spelled as the registry has them, and the
// labels Scryer would not define. Scryer refuses addTitle for an undefined
// tag, and defining one needs the catalog-settings permission, so one label
// that cannot be defined is dropped on its own; the others are still used.
// A label another request defined at the same moment counts as defined. Only
// a failure to reach Scryer is returned as an error, so the caller can retry
// the whole request rather than add the title untagged.
func (c *Client) EnsureTags(ctx context.Context, labels []string) (use, dropped []string, err error) {
	have, err := c.tagDefinitions(ctx)
	var gqlErr *GraphQLError
	if errors.As(err, &gqlErr) {
		return nil, labels, nil
	}
	if err != nil {
		return nil, nil, err
	}
	const q = `mutation SiloCreateTag($input: CreateTitleTagDefinitionInput!) { createTitleTagDefinition(input: $input) { __typename } }`
	for _, label := range labels {
		if stored, ok := have[strings.ToLower(label)]; ok {
			use = append(use, stored)
			continue
		}
		in := map[string]any{"label": label, "description": "Added by the Silo request plugin"}
		err := c.do(ctx, q, map[string]any{"input": in}, &struct{}{})
		if err != nil && !errors.As(err, &gqlErr) {
			return nil, nil, err
		}
		if err != nil {
			// Refused, or defined by a concurrent request first: look again.
			if have, err = c.tagDefinitions(ctx); err != nil && !errors.As(err, &gqlErr) {
				return nil, nil, err
			}
			if stored, ok := have[strings.ToLower(label)]; ok {
				use = append(use, stored)
				continue
			}
			dropped = append(dropped, label)
			continue
		}
		use = append(use, label)
	}
	return use, dropped, nil
}

// tagDefinitions returns the registry's labels keyed by their lowercase form.
func (c *Client) tagDefinitions(ctx context.Context) (map[string]string, error) {
	var out struct {
		Definitions []struct {
			Label string `json:"label"`
		} `json:"titleTagDefinitions"`
	}
	if err := c.do(ctx, `query SiloTags { titleTagDefinitions { label } }`, nil, &out); err != nil {
		return nil, err
	}
	have := make(map[string]string, len(out.Definitions))
	for _, d := range out.Definitions {
		have[strings.ToLower(d.Label)] = d.Label
	}
	return have, nil
}

// UpdateTitleTags adds already defined tags to a title.
func (c *Client) UpdateTitleTags(ctx context.Context, titleID string, add []string) error {
	const q = `mutation SiloTagTitle($input: UpdateTitleTagsInput!) { updateTitleTags(input: $input) { id } }`
	return c.do(ctx, q, map[string]any{"input": map[string]any{"titleIds": []string{titleID}, "add": add}}, &struct{}{})
}

// SetTitleMonitored switches a title on.
func (c *Client) SetTitleMonitored(ctx context.Context, titleID string) error {
	const q = `mutation SiloMonitorTitle($input: SetTitleMonitoredInput!) { setTitleMonitored(input: $input) { id } }`
	return c.do(ctx, q, map[string]any{"input": map[string]any{"titleId": titleID, "monitored": true}}, &struct{}{})
}

// FindTitle returns the id of the title of the facet with this TMDB id that
// Scryer already has, or "". Scryer's title list cannot be filtered by external
// id, so it is read page by page.
func (c *Client) FindTitle(ctx context.Context, facet, tmdbID string) (string, error) {
	if tmdbID == "" {
		return "", nil
	}
	const page = 200
	for offset := 0; ; offset += page {
		var out struct {
			Titles struct {
				Items []struct {
					ID          string `json:"id"`
					ExternalIDs []struct {
						Source string `json:"source"`
						Value  string `json:"value"`
					} `json:"externalIds"`
				} `json:"items"`
				HasMore bool `json:"hasMore"`
			} `json:"titles"`
		}
		const q = `query SiloFind($facet: MediaFacetValue, $limit: Int, $offset: Int) { titles(facet: $facet, limit: $limit, offset: $offset) { items { id externalIds { source value } } hasMore } }`
		if err := c.do(ctx, q, map[string]any{"facet": facet, "limit": page, "offset": offset}, &out); err != nil {
			return "", err
		}
		for _, t := range out.Titles.Items {
			for _, e := range t.ExternalIDs {
				if e.Source == "tmdb" && e.Value == tmdbID {
					return t.ID, nil
				}
			}
		}
		if !out.Titles.HasMore {
			return "", nil
		}
	}
}

// FindSeries is FindTitle for a series or anime: it looks in the library of the
// given facet first, then in the other one (SERIES or ANIME).
func (c *Client) FindSeries(ctx context.Context, facet, tmdbID string) (string, error) {
	other := "SERIES"
	if facet == "SERIES" {
		other = "ANIME"
	}
	for _, f := range []string{facet, other} {
		id, err := c.FindTitle(ctx, f, tmdbID)
		if err != nil || id != "" {
			return id, err
		}
	}
	return "", nil
}

// Search starts Scryer's automatic search for a title.
func (c *Client) Search(ctx context.Context, titleID string) error {
	const q = `mutation SiloSearch($input: TriggerAcquisitionSearchInput!) { triggerAcquisitionSearch(input: $input) { id } }`
	return c.do(ctx, q, map[string]any{"input": map[string]any{"intent": "AUTOMATIC", "titleId": titleID}}, &struct{}{})
}

// Title loads a title's files and downloads; it returns nil when Scryer no
// longer has the title.
func (c *Client) Title(ctx context.Context, id string) (*Title, error) {
	var out struct {
		Title *Title `json:"title"`
	}
	const q = `query SiloTitle($id: ID!) { title(id: $id) { id monitored mediaFiles { id } collections { collectionType collectionIndex monitored episodesOwned episodesMonitored } downloadQueueItems(includeImportActivity: true) { displayState progressPercent sizeBytes remainingSeconds } } }`
	if err := c.do(ctx, q, map[string]any{"id": id}, &out); err != nil {
		return nil, err
	}
	return out.Title, nil
}

// MissingMonitoredEpisodes counts the monitored episodes without a file in the
// given seasons of a title. The collection counts cannot say it: the episodes
// already owned are not monitored when a request leaves them alone, so "owned"
// and "monitored" are two different sets.
func (c *Client) MissingMonitoredEpisodes(ctx context.Context, titleID string, seasons []int32) (int, error) {
	var out struct {
		Title *struct {
			Collections []Collection `json:"collections"`
		} `json:"title"`
	}
	const q = `query SiloMissing($id: ID!) { title(id: $id) { collections { collectionType collectionIndex episodes { monitored mediaFiles { id } } } } }`
	if err := c.do(ctx, q, map[string]any{"id": titleID}, &out); err != nil {
		return 0, err
	}
	if out.Title == nil {
		return 0, fmt.Errorf("%w: the title is gone", ErrSeasonNotFound)
	}
	missing := 0
	for _, col := range out.Title.Collections {
		for _, n := range seasons {
			if col.Index != strconv.Itoa(int(n)) {
				continue
			}
			for _, e := range col.Episodes {
				if e.Monitored && len(e.MediaFiles) == 0 {
					missing++
				}
			}
		}
	}
	return missing, nil
}

// ErrSeasonNotFound means the title has no season with the requested number.
var ErrSeasonNotFound = errors.New("scryer has no such season")

// ErrNotLoaded means Scryer had not finished loading a new title's seasons when
// the wait ended. It is a matter of time, so the caller retries later.
var ErrNotLoaded = errors.New("scryer has not finished loading the title's seasons")

// SeasonPoll is how long MonitorSeasons waits between reads of a title that has
// just been created, while Scryer is still loading its seasons.
var SeasonPoll = 2 * time.Second

// seasonPolls is how many times it reads before giving up on a new title.
const seasonPolls = 10

// collections reads a title's seasons with their episodes. For a title the
// plugin has just created Scryer loads them a few seconds after addTitle
// returns, so they are read again until the requested ones are there.
func (c *Client) collections(ctx context.Context, titleID string, seasons []int32, newTitle bool) ([]Collection, error) {
	for attempt := 0; ; attempt++ {
		var out struct {
			Title *struct {
				MetadataFetchedAt *string      `json:"metadataFetchedAt"`
				Collections       []Collection `json:"collections"`
			} `json:"title"`
		}
		const q = `query SiloSeasons($id: ID!) { title(id: $id) { metadataFetchedAt collections { id collectionType collectionIndex monitored episodesMonitored episodesTotal episodes { id monitored mediaFiles { id } } } } }`
		if err := c.do(ctx, q, map[string]any{"id": titleID}, &out); err != nil {
			return nil, err
		}
		if out.Title == nil {
			return nil, fmt.Errorf("%w: the title is gone", ErrSeasonNotFound)
		}
		// Scryer sets metadataFetchedAt in the same moment the seasons appear, so
		// once it is set a season that is still missing is not coming.
		if !newTitle || loaded(out.Title.Collections, seasons) || (out.Title.MetadataFetchedAt != nil && len(out.Title.Collections) > 0) {
			return out.Title.Collections, nil
		}
		if attempt >= seasonPolls {
			if out.Title.MetadataFetchedAt == nil {
				return nil, ErrNotLoaded
			}
			return out.Title.Collections, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(SeasonPoll):
		}
	}
}

// MonitorSeasons makes sure the given seasons of a title will be downloaded and
// reports whether it had to change anything, and which of the seasons Scryer
// does not have (yet): they are left out, the others are still monitored. It
// fails with ErrSeasonNotFound only when none of the seasons exists.
//
// A new title (newTitle) has nothing yet, so each requested season is switched
// on with all its episodes. In a title Scryer already had, only the episodes
// that have no file and are not monitored are switched on, one by one: the rest
// is already managed and is left as it is. Monitoring an episode also switches
// on its season and its title.
//
// Scryer's addTitle options are not used for any of this: on a title Scryer
// already has they replace its stored monitor type and clear its season
// selection.
func (c *Client) MonitorSeasons(ctx context.Context, titleID string, seasons []int32, newTitle bool) (changed bool, missing []int32, err error) {
	cols, err := c.collections(ctx, titleID, seasons, newTitle)
	if err != nil {
		return false, nil, err
	}
	var wanted []Collection
	for _, n := range seasons {
		i := slices.IndexFunc(cols, func(col Collection) bool {
			return col.Index == strconv.Itoa(int(n)) && (col.Type == "SEASON" || col.Type == "SPECIALS")
		})
		if i < 0 {
			missing = append(missing, n)
			continue
		}
		wanted = append(wanted, cols[i])
	}
	if len(wanted) == 0 {
		return false, missing, fmt.Errorf("%w: %s", ErrSeasonNotFound, SeasonList(missing))
	}
	for _, col := range wanted {
		switch {
		case newTitle:
			if col.Monitored && !col.unmonitoredEpisodes() {
				continue
			}
			if err := c.setCollectionMonitored(ctx, col.ID, true); err != nil {
				return changed, missing, err
			}
			changed = true
		default:
			// A season with no episodes yet (an upcoming one) has nothing to
			// monitor one by one: monitoring the season itself, and so the title,
			// is what makes its episodes download once they appear.
			if len(col.Episodes) == 0 && !col.Monitored {
				if err := c.setCollectionMonitored(ctx, col.ID, true); err != nil {
					return changed, missing, err
				}
				changed = true
			}
			for _, e := range col.Episodes {
				if len(e.MediaFiles) > 0 || e.Monitored {
					continue
				}
				const set = `mutation SiloMonitorEpisode($input: SetEpisodeMonitoredInput!) { setEpisodeMonitored(input: $input) { __typename } }`
				if err := c.do(ctx, set, map[string]any{"input": map[string]any{"episodeId": e.ID, "monitored": true}}, &struct{}{}); err != nil {
					return changed, missing, err
				}
				changed = true
			}
		}
	}
	return changed, missing, nil
}

// SeasonList reads season numbers for a message: "season 5" or "seasons 4, 5".
func SeasonList(seasons []int32) string {
	parts := make([]string, len(seasons))
	for i, n := range seasons {
		parts[i] = strconv.Itoa(int(n))
	}
	if len(parts) == 1 {
		return "season " + parts[0]
	}
	return "seasons " + strings.Join(parts, ", ")
}

func (c *Client) setCollectionMonitored(ctx context.Context, id string, monitored bool) error {
	const set = `mutation SiloMonitorSeason($input: SetCollectionMonitoredInput!) { setCollectionMonitored(input: $input) { __typename } }`
	return c.do(ctx, set, map[string]any{"input": map[string]any{"collectionId": id, "monitored": monitored}}, &struct{}{})
}

// MonitorSpecials monitors the specials of a title the plugin has just created,
// if it has any.
func (c *Client) MonitorSpecials(ctx context.Context, titleID string) error {
	cols, err := c.collections(ctx, titleID, nil, true)
	if errors.Is(err, ErrNotLoaded) {
		return nil // nothing to monitor yet; the title itself is already in place
	}
	if err != nil {
		return err
	}
	for _, col := range cols {
		if col.Type == "SPECIALS" && (!col.Monitored || col.unmonitoredEpisodes()) && col.EpisodesTotal != nil && *col.EpisodesTotal > 0 {
			return c.setCollectionMonitored(ctx, col.ID, true)
		}
	}
	return nil
}

// loaded reports whether Scryer has loaded the title's seasons: some are there,
// and every requested one is among them.
func loaded(collections []Collection, seasons []int32) bool {
	if len(collections) == 0 {
		return false
	}
	for _, n := range seasons {
		found := false
		for _, col := range collections {
			if col.Index == strconv.Itoa(int(n)) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// SeasonStarts lists the regular seasons of a title with the air date of their
// first episode, for matching them to another catalog's seasons. For a title
// just created it waits, like MonitorSeasons, until Scryer has loaded them.
func (c *Client) SeasonStarts(ctx context.Context, titleID string, newTitle bool) ([]seasonmap.Season, error) {
	for attempt := 0; ; attempt++ {
		var out struct {
			Title *struct {
				Collections []struct {
					Type     string `json:"collectionType"`
					Index    string `json:"collectionIndex"`
					Episodes []struct {
						AirDate string `json:"airDate"`
					} `json:"episodes"`
				} `json:"collections"`
			} `json:"title"`
		}
		const q = `query SiloSeasonDates($id: ID!) { title(id: $id) { collections { collectionType collectionIndex episodes { airDate } } } }`
		if err := c.do(ctx, q, map[string]any{"id": titleID}, &out); err != nil {
			return nil, err
		}
		if out.Title == nil {
			return nil, errors.New("the title is gone")
		}
		if len(out.Title.Collections) > 0 || !newTitle || attempt >= seasonPolls {
			var seasons []seasonmap.Season
			for _, col := range out.Title.Collections {
				n, err := strconv.Atoi(col.Index)
				if col.Type != "SEASON" || err != nil {
					continue
				}
				s := seasonmap.Season{Number: n, Episodes: len(col.Episodes)}
				for _, e := range col.Episodes {
					if d, err := time.Parse(time.DateOnly, e.AirDate); err == nil && (s.Start.IsZero() || d.Before(s.Start)) {
						s.Start = d
					}
				}
				seasons = append(seasons, s)
			}
			return seasons, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(SeasonPoll):
		}
	}
}

func (c *Client) do(ctx context.Context, query string, vars map[string]any, out any) error {
	if c.BaseURL == "" {
		return errors.New("enter Scryer's URL")
	}
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/graphql", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("scryer answered HTTP %d", resp.StatusCode)
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode scryer response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		gqlErr := &GraphQLError{}
		for _, e := range envelope.Errors {
			gqlErr.Messages = append(gqlErr.Messages, e.Message)
		}
		return gqlErr
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return errors.New("scryer returned no data")
	}
	return json.Unmarshal(envelope.Data, out)
}
