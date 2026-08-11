package matrix

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"sync"

	// Registered for image.DecodeConfig, which reads dimensions without
	// decoding the pixels.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/jellyfin"
)

// artworkSize is the pixel size requested from Jellyfin for chat covers.
const artworkSize = 800

// uploaded is one cover already in the homeserver's media repo.
type uploaded struct {
	URI    id.ContentURIString
	Mime   string
	Size   int
	Width  int
	Height int
}

// artworkCache remembers uploads by Jellyfin image ID, so playing a 20-track
// album uploads its cover once rather than twenty times.
type artworkCache struct {
	mu    sync.Mutex
	byID  map[string]uploaded
	limit int
}

func newArtworkCache(limit int) *artworkCache {
	return &artworkCache{byID: make(map[string]uploaded), limit: limit}
}

func (c *artworkCache) get(id string) (uploaded, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.byID[id]
	return item, ok
}

func (c *artworkCache) put(id string, item uploaded) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.byID) >= c.limit {
		// Covers are cheap to re-upload; drop everything rather than track
		// recency for what is only an optimisation.
		clear(c.byID)
	}
	c.byID[id] = item
}

// sendNowPlaying announces a track, as an image with the details as its
// caption when the track has cover art, and as plain text otherwise.
func (b *Bot) sendNowPlaying(ctx context.Context, item jellyfin.Item, caption string) {
	if !b.cfg.Audio.ShowArtChat() {
		b.send(ctx, caption, "")
		return
	}

	art, err := b.uploadArtwork(ctx, item)
	if err != nil {
		if err != jellyfin.ErrNoArtwork {
			b.client.Log.Warn().Err(err).Str("track", item.Name).Msg("could not attach album art")
		}
		b.send(ctx, caption, "")
		return
	}

	// A media event with both filename and body is the spec's caption form:
	// clients that support it render the image with the text below, and older
	// ones fall back to showing the caption as the file's name.
	content := event.MessageEventContent{
		MsgType:  event.MsgImage,
		Body:     caption,
		FileName: coverFileName(item),
		URL:      art.URI,
		Info: &event.FileInfo{
			MimeType: art.Mime,
			Size:     art.Size,
			Width:    art.Width,
			Height:   art.Height,
		},
	}
	if _, err := b.client.SendMessageEvent(ctx, b.roomID, event.EventMessage, &content); err != nil {
		b.client.Log.Err(err).Msg("failed to send now-playing image")
		b.send(ctx, caption, "")
	}
}

// uploadArtwork fetches a track's cover and uploads it, reusing a previous
// upload of the same image.
func (b *Bot) uploadArtwork(ctx context.Context, item jellyfin.Item) (uploaded, error) {
	if item.ArtworkID == "" {
		return uploaded{}, jellyfin.ErrNoArtwork
	}
	if cached, ok := b.artwork.get(item.ArtworkID); ok {
		return cached, nil
	}

	data, mime, err := b.jf.Artwork(ctx, item, artworkSize)
	if err != nil {
		return uploaded{}, err
	}

	resp, err := b.client.UploadBytesWithName(ctx, data, mime, coverFileName(item))
	if err != nil {
		return uploaded{}, fmt.Errorf("upload artwork: %w", err)
	}

	art := uploaded{URI: resp.ContentURI.CUString(), Mime: mime, Size: len(data)}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		art.Width, art.Height = cfg.Width, cfg.Height
	}

	b.artwork.put(item.ArtworkID, art)
	return art, nil
}

func coverFileName(item jellyfin.Item) string {
	name := item.Album
	if name == "" {
		name = item.Name
	}
	if name == "" {
		return "cover.jpg"
	}
	return sanitizeFileName(name) + ".jpg"
}

// sanitizeFileName keeps a filename readable and free of path separators.
func sanitizeFileName(s string) string {
	var out []rune
	for _, r := range s {
		switch {
		case r == '/' || r == '\\' || r == 0:
			out = append(out, '-')
		case r < 0x20:
			// Skip control characters.
		default:
			out = append(out, r)
		}
		if len(out) >= 64 {
			break
		}
	}
	if len(out) == 0 {
		return "cover"
	}
	return string(out)
}
