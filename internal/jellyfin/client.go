// Package jellyfin wraps the generated Jellyfin API client with the small
// surface the bot needs: searching the music library, expanding containers
// (artist, album, playlist) into tracks, and building stream URLs for ffmpeg.
package jellyfin

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	api "github.com/sj14/jellyfin-go/api"
)

// Kind is the type of a library item the bot can show or play.
type Kind string

const (
	KindArtist   Kind = "artist"
	KindAlbum    Kind = "album"
	KindTrack    Kind = "track"
	KindPlaylist Kind = "playlist"
)

// AllKinds is the default search scope, in display order.
var AllKinds = []Kind{KindArtist, KindAlbum, KindTrack, KindPlaylist}

// ParseKind maps a user-supplied word to a Kind.
func ParseKind(s string) (Kind, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "artist", "artists":
		return KindArtist, true
	case "album", "albums":
		return KindAlbum, true
	case "track", "tracks", "song", "songs", "title":
		return KindTrack, true
	case "playlist", "playlists":
		return KindPlaylist, true
	}
	return "", false
}

func (k Kind) itemKind() api.BaseItemKind {
	switch k {
	case KindArtist:
		return api.BASEITEMKIND_MUSIC_ARTIST
	case KindAlbum:
		return api.BASEITEMKIND_MUSIC_ALBUM
	case KindPlaylist:
		return api.BASEITEMKIND_PLAYLIST
	default:
		return api.BASEITEMKIND_AUDIO
	}
}

// Item is a library entry: an artist, album, playlist or a single track.
type Item struct {
	ID       string
	Kind     Kind
	Name     string
	Artist   string
	Album    string
	Duration time.Duration
}

// Describe renders the item for chat output.
func (i Item) Describe() string {
	switch i.Kind {
	case KindTrack:
		s := i.Name
		if i.Artist != "" {
			s = i.Artist + " – " + i.Name
		}
		if i.Album != "" {
			s += " (" + i.Album + ")"
		}
		if i.Duration > 0 {
			s += " [" + FormatDuration(i.Duration) + "]"
		}
		return s
	case KindAlbum:
		if i.Artist != "" {
			return i.Artist + " – " + i.Name
		}
		return i.Name
	default:
		return i.Name
	}
}

// FormatDuration renders a duration as m:ss or h:mm:ss.
func FormatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int(d/time.Minute) % 60
	s := int(d/time.Second) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// Client talks to a Jellyfin server as a single API-key service account.
type Client struct {
	api    *api.APIClient
	server string
	apiKey string
	userID string
}

// New builds a Jellyfin client. server must include scheme and port.
func New(server, apiKey, userID string) *Client {
	cfg := &api.Configuration{
		Servers:       api.ServerConfigurations{{URL: server}},
		DefaultHeader: map[string]string{"Authorization": fmt.Sprintf(`MediaBrowser Token="%s"`, apiKey)},
	}
	return &Client{
		api:    api.NewAPIClient(cfg),
		server: strings.TrimSuffix(server, "/"),
		apiKey: apiKey,
		userID: userID,
	}
}

// Ping verifies the server is reachable and the credentials work. It also
// resolves jellyfin.user_id if it was given as a username rather than an ID.
func (c *Client) Ping(ctx context.Context) error {
	_, _, err := c.api.SystemAPI.GetPublicSystemInfo(ctx).Execute()
	if err != nil {
		return fmt.Errorf("jellyfin unreachable: %w", err)
	}
	if err := c.resolveUser(ctx); err != nil {
		return err
	}
	if _, _, err := c.api.ItemsAPI.GetItems(ctx).UserId(c.userID).Limit(1).Execute(); err != nil {
		return fmt.Errorf("jellyfin credentials or user_id rejected: %w", err)
	}
	return nil
}

// UserID is the resolved Jellyfin user ID in use.
func (c *Client) UserID() string { return c.userID }

// resolveUser turns a configured username into its user ID. Jellyfin's item
// APIs only accept IDs, but a username is the obvious thing to put in a config
// file, so accept either.
func (c *Client) resolveUser(ctx context.Context) error {
	if isUserID(c.userID) {
		return nil
	}
	users, _, err := c.api.UserAPI.GetUsers(ctx).Execute()
	if err != nil {
		return fmt.Errorf("look up jellyfin user %q: %w", c.userID, err)
	}
	var names []string
	for _, user := range users {
		name := str(user.Name.Get())
		names = append(names, name)
		if strings.EqualFold(name, c.userID) && user.Id != nil {
			c.userID = *user.Id
			return nil
		}
	}
	return fmt.Errorf("no jellyfin user named %q; known users: %s", c.userID, strings.Join(names, ", "))
}

// isUserID reports whether s looks like a Jellyfin user ID: a UUID with the
// dashes stripped, as Jellyfin renders them.
func isUserID(s string) bool {
	s = strings.ReplaceAll(s, "-", "")
	if len(s) != 32 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// Search returns up to limit items of each requested kind matching term.
func (c *Client) Search(ctx context.Context, kinds []Kind, term string, limit int) ([]Item, error) {
	if len(kinds) == 0 {
		kinds = AllKinds
	}
	var out []Item
	for _, kind := range kinds {
		res, _, err := c.api.ItemsAPI.GetItems(ctx).
			UserId(c.userID).
			SearchTerm(term).
			IncludeItemTypes([]api.BaseItemKind{kind.itemKind()}).
			Recursive(true).
			Limit(int32(limit)).
			SortBy([]api.ItemSortBy{api.ITEMSORTBY_SORT_NAME}).
			Execute()
		if err != nil {
			return nil, fmt.Errorf("search %s: %w", kind, err)
		}
		for _, dto := range res.Items {
			out = append(out, toItem(dto, kind))
		}
	}
	return out, nil
}

// Tracks expands an item into the tracks it should enqueue. A track expands to
// itself; artists, albums and playlists expand to their audio children.
func (c *Client) Tracks(ctx context.Context, item Item) ([]Item, error) {
	switch item.Kind {
	case KindTrack:
		return []Item{item}, nil

	case KindPlaylist:
		// Use the playlist API so the playlist's own ordering is preserved.
		res, _, err := c.api.PlaylistsAPI.GetPlaylistItems(ctx, item.ID).
			UserId(c.userID).
			Execute()
		if err != nil {
			return nil, fmt.Errorf("playlist items: %w", err)
		}
		return audioItems(res.Items), nil

	case KindAlbum:
		res, _, err := c.api.ItemsAPI.GetItems(ctx).
			UserId(c.userID).
			ParentId(item.ID).
			IncludeItemTypes([]api.BaseItemKind{api.BASEITEMKIND_AUDIO}).
			Recursive(true).
			SortBy([]api.ItemSortBy{api.ITEMSORTBY_INDEX_NUMBER}).
			Execute()
		if err != nil {
			return nil, fmt.Errorf("album tracks: %w", err)
		}
		return audioItems(res.Items), nil

	case KindArtist:
		// ParentId does not work for artists; ArtistIds is the correct filter.
		res, _, err := c.api.ItemsAPI.GetItems(ctx).
			UserId(c.userID).
			ArtistIds([]string{item.ID}).
			IncludeItemTypes([]api.BaseItemKind{api.BASEITEMKIND_AUDIO}).
			Recursive(true).
			SortBy([]api.ItemSortBy{api.ITEMSORTBY_ALBUM, api.ITEMSORTBY_INDEX_NUMBER}).
			Execute()
		if err != nil {
			return nil, fmt.Errorf("artist tracks: %w", err)
		}
		return audioItems(res.Items), nil
	}
	return nil, fmt.Errorf("cannot play item kind %q", item.Kind)
}

// StreamURL returns a direct-download URL ffmpeg can read. static=true asks
// Jellyfin for the original file rather than a transcode; ffmpeg handles
// whatever container and codec comes back.
func (c *Client) StreamURL(trackID string) string {
	q := url.Values{}
	q.Set("static", "true")
	q.Set("api_key", c.apiKey)
	q.Set("userId", c.userID)
	return fmt.Sprintf("%s/Audio/%s/stream?%s", c.server, url.PathEscape(trackID), q.Encode())
}

func audioItems(dtos []api.BaseItemDto) []Item {
	out := make([]Item, 0, len(dtos))
	for _, dto := range dtos {
		if dto.Type == nil || *dto.Type != api.BASEITEMKIND_AUDIO {
			continue
		}
		out = append(out, toItem(dto, KindTrack))
	}
	return out
}

func toItem(dto api.BaseItemDto, kind Kind) Item {
	item := Item{Kind: kind, Name: str(dto.Name.Get())}
	if dto.Id != nil {
		item.ID = *dto.Id
	}
	item.Album = str(dto.Album.Get())
	if len(dto.Artists) > 0 {
		item.Artist = strings.Join(dto.Artists, ", ")
	} else {
		item.Artist = str(dto.AlbumArtist.Get())
	}
	if ticks := dto.RunTimeTicks.Get(); ticks != nil && *ticks > 0 {
		// Jellyfin ticks are 100ns units.
		item.Duration = time.Duration(*ticks) * 100 * time.Nanosecond
	}
	return item
}

func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
