package matrix

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	// The pure-Go SQLite driver, registered as "sqlite". The one mautrix
	// reaches for by default is cgo, and the bot builds with CGO_ENABLED=0.
	_ "modernc.org/sqlite"

	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/config"
	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/rtc"
)

// Crypto is the bot's end-to-end encryption: Megolm for the room, Olm for the
// to-device messages that carry its call media key.
type Crypto struct {
	helper *cryptohelper.CryptoHelper
	client *mautrix.Client
	log    zerolog.Logger

	// sticky remembers the stickiness of encrypted events until they are
	// decrypted; see rememberSticky.
	stickyMu sync.Mutex
	sticky   map[id.EventID]stickyNote

	// onDecryptError is told about events that could not be decrypted.
	onDecryptError func(evt *event.Event, err error)
}

type stickyNote struct {
	sticky *event.Sticky
	seen   time.Time
}

// volatileSyncStore keeps the sync token in memory.
//
// The crypto helper would otherwise move it into its database, and the bot would
// resume from where it stopped on every restart. The bot depends on starting
// from a full sync instead: sticky call memberships are only replayed in full
// to a sync without a token, and a bot resuming mid-call would sit unaware of
// everybody already in it. The crypto helper only swaps out the stock memory
// store, so wrapping it is what keeps it.
type volatileSyncStore struct{ *mautrix.MemorySyncStore }

// SetupCrypto opens the crypto store and attaches end-to-end encryption to
// client. After it, messages to an encrypted room are encrypted and encrypted
// events are decrypted and dispatched like any other.
//
// It must run before the sync handlers are registered, so the events it
// decrypts reach them.
func SetupCrypto(ctx context.Context, client *mautrix.Client, cfg config.Crypto) (*Crypto, error) {
	db, err := dbutil.NewWithDialect(sqliteURI(cfg.Store), "sqlite")
	if err != nil {
		return nil, fmt.Errorf("open crypto store %s: %w", cfg.Store, err)
	}
	helper, err := cryptohelper.NewCryptoHelper(client, []byte(cfg.PickleKey), db)
	if err != nil {
		return nil, fmt.Errorf("set up encryption: %w", err)
	}
	client.Store = volatileSyncStore{mautrix.NewMemorySyncStore()}

	c := &Crypto{
		helper: helper,
		client: client,
		log:    client.Log.With().Str("component", "crypto").Logger(),
		sticky: make(map[id.EventID]stickyNote),
	}
	syncer, ok := client.Syncer.(*mautrix.DefaultSyncer)
	if !ok {
		return nil, fmt.Errorf("unexpected syncer type %T", client.Syncer)
	}
	// Registered ahead of the helper's own handler for the same type, so the
	// note is there by the time the decrypted event is dispatched.
	syncer.OnEventType(event.EventEncrypted, c.rememberSticky)
	helper.CustomPostDecrypt = c.dispatchDecrypted
	helper.DecryptErrorCallback = func(evt *event.Event, err error) {
		if c.onDecryptError != nil {
			c.onDecryptError(evt, err)
		}
	}

	if err := helper.Init(ctx); err != nil {
		return nil, fmt.Errorf("start encryption (is %s from another device?): %w", cfg.Store, err)
	}
	client.Crypto = helper
	return c, nil
}

// sqliteURI opens path with foreign keys on, as mautrix's schema expects, and
// immediate transactions, so two writers wait for each other instead of
// failing.
func sqliteURI(path string) string {
	return "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate"
}

// Close releases the crypto store.
func (c *Crypto) Close() error { return c.helper.Close() }

// rememberSticky notes the stickiness of an encrypted event.
//
// Stickiness lives on the outside of the encrypted event, and mautrix builds
// the decrypted one without it. A call membership that arrived encrypted would
// then look like one with no lifetime, and be ignored.
func (c *Crypto) rememberSticky(_ context.Context, evt *event.Event) {
	if evt.Sticky == nil {
		return
	}
	now := time.Now()
	c.stickyMu.Lock()
	defer c.stickyMu.Unlock()
	for eventID, note := range c.sticky {
		// Anything still here after the longest stickiness there is never
		// decrypted.
		if now.Sub(note.seen) > event.MaxStickyDuration {
			delete(c.sticky, eventID)
		}
	}
	c.sticky[evt.ID] = stickyNote{sticky: evt.Sticky, seen: now}
}

// dispatchDecrypted hands a decrypted event to the sync handlers, with the
// stickiness its encrypted form carried.
func (c *Crypto) dispatchDecrypted(ctx context.Context, evt *event.Event) {
	c.stickyMu.Lock()
	if note, ok := c.sticky[evt.ID]; ok {
		evt.Sticky = note.sticky
		delete(c.sticky, evt.ID)
	}
	c.stickyMu.Unlock()
	c.client.Syncer.(mautrix.DispatchableSyncer).Dispatch(ctx, evt)
}

// OnDecryptError sets what is told about events that could not be decrypted.
func (c *Crypto) OnDecryptError(fn func(evt *event.Event, err error)) { c.onDecryptError = fn }

// RefreshDevices re-reads the device lists of everyone in the room.
//
// The bot starts every run from a full sync, and a full sync does not say
// whose devices changed while it was away. Without this, a device somebody
// added in the meantime would get neither the room keys nor the call key.
func (c *Crypto) RefreshDevices(ctx context.Context, roomID id.RoomID) error {
	resp, err := c.client.JoinedMembers(ctx, roomID)
	if err != nil {
		return fmt.Errorf("list room members: %w", err)
	}
	users := make([]id.UserID, 0, len(resp.Joined))
	for user := range resp.Joined {
		users = append(users, user)
	}
	if _, err := c.helper.Machine().FetchKeys(ctx, users, true); err != nil {
		return fmt.Errorf("fetch device keys: %w", err)
	}
	return nil
}

// VerifyWithRecoveryKeyFile cross-signs the bot's device with the account's
// cross-signing keys, unlocked from secret storage with the recovery key in
// path. Clients that trust the bot's account then trust this device too.
func (c *Crypto) VerifyWithRecoveryKeyFile(ctx context.Context, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read recovery key: %w", err)
	}
	if err := c.helper.Machine().VerifyWithRecoveryKey(ctx, strings.TrimSpace(string(data))); err != nil {
		return fmt.Errorf("verify with recovery key: %w", err)
	}
	return nil
}

// ErrCrossSigningExists means the account already has a cross-signing
// identity. Making a new one would reset it, and everyone who had verified
// the bot would see its identity change, so the existing recovery key is the
// way in.
var ErrCrossSigningExists = errors.New("the account already has cross-signing keys; put its recovery key in matrix.crypto.recovery_key_file instead")

// SetUpCrossSigning gives the account a cross-signing identity, signs the bot's
// device with it, and returns the recovery key that unlocks it from secret
// storage. Uploading cross-signing keys needs user-interactive auth, hence the
// password.
func (c *Crypto) SetUpCrossSigning(ctx context.Context, password string) (string, error) {
	mach := c.helper.Machine()
	existing, err := mach.GetOwnCrossSigningPublicKeys(ctx)
	if err != nil {
		return "", fmt.Errorf("look up cross-signing keys: %w", err)
	}
	if existing != nil {
		return "", ErrCrossSigningExists
	}
	recoveryKey, _, err := mach.GenerateAndUploadCrossSigningKeysWithPassword(ctx, password, "")
	if err != nil {
		return "", fmt.Errorf("upload cross-signing keys: %w", err)
	}
	if err := mach.SignOwnDevice(ctx, mach.OwnIdentity()); err != nil {
		return recoveryKey, fmt.Errorf("sign own device: %w", err)
	}
	if err := mach.SignOwnMasterKey(ctx); err != nil {
		return recoveryKey, fmt.Errorf("sign own master key: %w", err)
	}
	return recoveryKey, nil
}

// encryptionKeysType is the to-device event Element Call shares media keys
// with.
var encryptionKeysType = event.Type{Type: "io.element.call.encryption_keys", Class: event.ToDeviceEventType}

// mediaKeyContent is an io.element.call.encryption_keys payload, as
// matrix-js-sdk's ToDeviceKeyTransport writes it.
type mediaKeyContent struct {
	Keys    mediaKeyEntry   `json:"keys"`
	RoomID  id.RoomID       `json:"room_id"`
	Member  mediaKeyMember  `json:"member"`
	Session mediaKeySession `json:"session"`
	SentTS  int64           `json:"sent_ts"`
}

type mediaKeyEntry struct {
	Index int    `json:"index"`
	Key   string `json:"key"`
}

type mediaKeyMember struct {
	ClaimedDeviceID id.DeviceID `json:"claimed_device_id"`
	ID              string      `json:"id"`
}

type mediaKeySession struct {
	CallID      string `json:"call_id"`
	Application string `json:"application"`
	Scope       string `json:"scope"`
}

// MediaKeySender sends the bot's call media key to devices in roomID's call.
//
// Each device gets one message per membership the bot is in the call on, each
// naming that membership. A client files a key under the member it belongs to,
// and which of the bot's memberships a client sees depends on which dialects it
// reads.
func (c *Crypto) MediaKeySender(roomID id.RoomID) rtc.KeySender {
	return func(ctx context.Context, targets []rtc.KeyTarget, memberIDs []string, index int, key []byte) ([]rtc.KeyTarget, error) {
		mach := c.helper.Machine()
		// Looked up one by one: EncryptToDevices gives up on the whole batch
		// when any one device is unknown.
		var devices, unreached []rtc.KeyTarget
		var lookupErr error
		for _, t := range targets {
			if _, err := mach.GetOrFetchDevice(ctx, t.User, t.Device); err != nil {
				c.log.Debug().Err(err).Str("user_id", t.User.String()).Str("device_id", t.Device.String()).
					Msg("cannot share the call key with a device whose keys are unknown")
				unreached = append(unreached, t)
				lookupErr = err
				continue
			}
			devices = append(devices, t)
		}

		encoded := base64.StdEncoding.EncodeToString(key)
		for _, memberID := range memberIDs {
			content := &mediaKeyContent{
				Keys:   mediaKeyEntry{Index: index, Key: encoded},
				RoomID: roomID,
				Member: mediaKeyMember{ClaimedDeviceID: c.client.DeviceID, ID: memberID},
				Session: mediaKeySession{
					Application: "m.call",
					Scope:       "m.room",
				},
				SentTS: time.Now().UnixMilli(),
			}
			req := &mautrix.ReqSendToDevice{Messages: make(map[id.UserID]map[id.DeviceID]*event.Content)}
			for _, t := range devices {
				if req.Messages[t.User] == nil {
					req.Messages[t.User] = make(map[id.DeviceID]*event.Content)
				}
				req.Messages[t.User][t.Device] = &event.Content{Parsed: content}
			}
			if len(req.Messages) == 0 {
				break
			}
			encrypted, err := mach.EncryptToDevices(ctx, encryptionKeysType, req)
			if err != nil {
				return nil, fmt.Errorf("encrypt call key: %w", err)
			}
			// A device with no Olm session to encrypt for — it has run out of
			// one-time keys — is left out of the request rather than failing it.
			for _, t := range devices {
				if encrypted.Messages[t.User][t.Device] == nil && !slices.Contains(unreached, t) {
					unreached = append(unreached, t)
				}
			}
			if _, err := c.client.SendToDevice(ctx, event.ToDeviceEncrypted, encrypted); err != nil {
				return nil, fmt.Errorf("send call key: %w", err)
			}
		}
		if len(unreached) > 0 && lookupErr == nil {
			lookupErr = errors.New("no olm session with some devices")
		}
		return unreached, lookupErr
	}
}

// SetCrypto attaches the room encryption, so the bot can tell people when it
// could not read what they sent.
func (b *Bot) SetCrypto(c *Crypto) {
	b.crypto = c
	c.OnDecryptError(b.reportUndecryptable)
}

// encrypted reports whether the bot's room is end-to-end encrypted, which
// decides how album art is uploaded.
func (b *Bot) encrypted(ctx context.Context) bool {
	if b.crypto == nil || b.client.StateStore == nil {
		return false
	}
	encrypted, err := b.client.StateStore.IsEncrypted(ctx, b.roomID)
	if err != nil {
		b.client.Log.Warn().Err(err).Msg("could not tell whether the room is encrypted")
		return false
	}
	return encrypted
}

// undecryptableNoticeInterval is how often one person is told the bot could
// not read their messages.
const undecryptableNoticeInterval = 10 * time.Minute

// reportUndecryptable tells somebody the bot could not read what they sent.
//
// The usual cause is on their side and invisible from it: a client set to send
// only to verified sessions, which has never verified the bot, simply leaves it
// out of the key share, and the command goes nowhere without a word.
func (b *Bot) reportUndecryptable(evt *event.Event, err error) {
	if evt.RoomID != b.roomID || evt.Sender == b.client.UserID || b.isHistorical(evt) {
		return
	}
	b.client.Log.Warn().Err(err).Str("sender", evt.Sender.String()).Str("event_id", evt.ID.String()).
		Msg("could not decrypt a message")

	now := time.Now()
	b.undecryptableMu.Lock()
	if b.undecryptable == nil {
		b.undecryptable = make(map[id.UserID]time.Time)
	}
	last, told := b.undecryptable[evt.Sender]
	if told && now.Sub(last) < undecryptableNoticeInterval {
		b.undecryptableMu.Unlock()
		return
	}
	b.undecryptable[evt.Sender] = now
	b.undecryptableMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name := b.displayName(ctx, evt.Sender)
	b.send(ctx, fmt.Sprintf("I could not decrypt a message from %s. If your client only shares keys with "+
		"verified sessions, verify my session (%s) or turn that off for this room.", name, b.client.DeviceID), "")
}
