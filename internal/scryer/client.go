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
	"strings"
	"time"
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
	Name      string   `json:"name"`
	Facet     string   `json:"facet"` // MOVIE, SERIES or ANIME
	Monitored bool     `json:"monitored"`
	Tags      []string `json:"tags"`
	Year      *int     `json:"year,omitempty"`
	TmdbID    *int64   `json:"tmdbId,omitempty"`
	TvdbID    *string  `json:"tvdbId,omitempty"`
	ImdbID    *string  `json:"imdbId,omitempty"`
}

// QueueItem is one download Scryer tracks for a title.
type QueueItem struct {
	DisplayState     string `json:"displayState"`
	ProgressPercent  int    `json:"progressPercent"`
	SizeBytes        *int64 `json:"sizeBytes"`
	RemainingSeconds *int   `json:"remainingSeconds"`
}

// Title is the state of one title in Scryer.
type Title struct {
	ID         string      `json:"id"`
	Monitored  bool        `json:"monitored"`
	MediaFiles []struct{}  `json:"mediaFiles"`
	Queue      []QueueItem `json:"downloadQueueItems"`
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

// AddTitle creates the title, or reuses it when Scryer already has it, and
// returns its ID.
func (c *Client) AddTitle(ctx context.Context, in AddTitle) (string, error) {
	var out struct {
		AddTitle struct {
			Title struct {
				ID string `json:"id"`
			} `json:"title"`
		} `json:"addTitle"`
	}
	const q = `mutation SiloAddTitle($input: AddTitleInput!) { addTitle(input: $input) { title { id } reusedExistingTitle } }`
	if err := c.do(ctx, q, map[string]any{"input": in}, &out); err != nil {
		return "", err
	}
	if out.AddTitle.Title.ID == "" {
		return "", errors.New("scryer returned no title id")
	}
	return out.AddTitle.Title.ID, nil
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
	const q = `query SiloTitle($id: ID!) { title(id: $id) { id monitored mediaFiles { id } downloadQueueItems(includeImportActivity: true) { displayState progressPercent sizeBytes remainingSeconds } } }`
	if err := c.do(ctx, q, map[string]any{"id": id}, &out); err != nil {
		return nil, err
	}
	return out.Title, nil
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
