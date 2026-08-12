package matrix

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"sync"

	// Registered for image decoding: DecodeConfig reads dimensions cheaply,
	// and Decode gives the pixels the blurhash is computed from.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/bbrks/go-blurhash"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/jellyfin"
)

const (
	// artworkSize is the pixel size requested from Jellyfin for the full cover.
	artworkSize = 800
	// thumbnailSize is what clients show before the full image loads. Jellyfin
	// scales server-side, so this is a second cheap fetch rather than a local
	// resize.
	thumbnailSize = 320
	// blurhash components. 4x3 is what Element uses; more components make a
	// longer hash for no visible gain at this size.
	blurhashX = 4
	blurhashY = 3
)

// uploadedImage is one uploaded picture in the homeserver's media repo.
type uploadedImage struct {
	URI    id.ContentURIString
	Mime   string
	Size   int
	Width  int
	Height int
}

// fileInfo renders the upload as the Matrix info block.
func (u uploadedImage) fileInfo() *event.FileInfo {
	return &event.FileInfo{
		MimeType: u.Mime,
		Size:     u.Size,
		Width:    u.Width,
		Height:   u.Height,
	}
}

// uploaded is a cover with everything a rich image event needs.
type uploaded struct {
	Full      uploadedImage
	Thumbnail *uploadedImage
	Blurhash  string
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
	info := art.Full.fileInfo()
	if art.Thumbnail != nil {
		info.ThumbnailURL = art.Thumbnail.URI
		info.ThumbnailInfo = art.Thumbnail.fileInfo()
	}
	if art.Blurhash != "" {
		info.Blurhash = art.Blurhash
		// Element reads the unstable key, so send both.
		info.AnoaBlurhash = art.Blurhash
	}

	content := event.MessageEventContent{
		MsgType:  event.MsgImage,
		Body:     caption,
		FileName: coverFileName(item),
		URL:      art.Full.URI,
		Info:     info,
	}
	if _, err := b.client.SendMessageEvent(ctx, b.roomID, event.EventMessage, &content); err != nil {
		b.client.Log.Err(err).Msg("failed to send now-playing image")
		b.send(ctx, caption, "")
	}
}

// uploadArtwork fetches a track's cover and its thumbnail, uploads both, and
// computes a blurhash. Repeat calls for the same cover reuse the upload.
func (b *Bot) uploadArtwork(ctx context.Context, item jellyfin.Item) (uploaded, error) {
	if item.ArtworkID == "" {
		return uploaded{}, jellyfin.ErrNoArtwork
	}
	if cached, ok := b.artwork.get(item.ArtworkID); ok {
		return cached, nil
	}

	_, full, err := b.fetchAndUpload(ctx, item, artworkSize)
	if err != nil {
		return uploaded{}, err
	}
	art := uploaded{Full: full}

	// The thumbnail and blurhash are decoration: a failure here should still
	// leave a perfectly good image event.
	if thumbData, thumb, err := b.fetchAndUpload(ctx, item, thumbnailSize); err != nil {
		b.client.Log.Debug().Err(err).Msg("no thumbnail for cover")
	} else {
		art.Thumbnail = &thumb
		if hash, err := computeBlurhash(thumbData); err != nil {
			b.client.Log.Debug().Err(err).Msg("could not compute blurhash")
		} else {
			art.Blurhash = hash
		}
	}

	b.artwork.put(item.ArtworkID, art)
	return art, nil
}

// fetchAndUpload uploads a cover at the given size and also returns the raw
// bytes, so the caller can derive a blurhash without fetching twice.
func (b *Bot) fetchAndUpload(ctx context.Context, item jellyfin.Item, size int) ([]byte, uploadedImage, error) {
	data, mime, err := b.jf.Artwork(ctx, item, size)
	if err != nil {
		return nil, uploadedImage{}, err
	}
	resp, err := b.client.UploadBytesWithName(ctx, data, mime, coverFileName(item))
	if err != nil {
		return nil, uploadedImage{}, fmt.Errorf("upload artwork: %w", err)
	}

	img := uploadedImage{URI: resp.ContentURI.CUString(), Mime: mime, Size: len(data)}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		img.Width, img.Height = cfg.Width, cfg.Height
	}
	return data, img, nil
}

// computeBlurhash renders the tiny colour placeholder clients show while the
// image loads.
func computeBlurhash(data []byte) (string, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("decode cover: %w", err)
	}
	hash, err := blurhash.Encode(blurhashX, blurhashY, img)
	if err != nil {
		return "", fmt.Errorf("encode blurhash: %w", err)
	}
	return hash, nil
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
