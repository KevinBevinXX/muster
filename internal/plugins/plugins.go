// Package plugins is muster's PLUGIN INTERFACE: out-of-process connectors that
// bring inbound messages from some upstream system to their owner's chief agent.
//
// # The shape
//
// muster NEVER calls a plugin. A plugin is a separate process holding a plugin
// token (`mpl_…`) its owner minted on /plugins; it POSTs each inbound message to
// `POST /plugin/v1/messages` and (a later PR) long-polls muster for replies its
// owner approved. The token reaches /plugin/v1/* and nothing else, and acts as its
// OWNER (authz.ViaPlugin), so row-level security confines a plugin to its owner's
// rows (migration 0015).
//
// # What happens to a message
//
// It is stored once per (plugin, external_id) — a repeat POST answers the stored
// row — and then handed to the owner's CHIEF agent as a fenced turn
// ([RenderMessage], internal/fence), in a chat session dedicated to that plugin
// ([Deliverer]). The delivery is attempted right after the POST; a delivery that
// provably pasted nothing leaves the message `queued` for a later attempt, and one
// that may have been pasted is `unconfirmed` and NEVER resent.
//
// Everything here is OFF unless the deployment sets MUSTER_PLUGINS=1: with it off
// no store is wired, every /plugin/* route answers 404 and /plugins is not drawn.
package plugins

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/fence"
)

// TokenPrefix starts every plugin token, so the plugin door can refuse every
// other credential (the hook token, a personal or runner token) before a
// database read — and every OTHER door can refuse a plugin token by its shape.
const TokenPrefix = "mpl_"

// Caps on one inbound message. A message over a cap is REFUSED (400), never
// truncated: the plugin is the only party that can decide what to drop, and a
// silently cut message would be delivered as if whole.
const (
	MaxBodyBytes      = 16 << 10
	MaxSenderRunes    = 200
	MaxSubjectRunes   = 200
	MaxExternalIDLen  = 200
	MaxLabelRunes     = 80
	maxRequestBytes   = 6*MaxBodyBytes + 12*MaxSenderRunes + 12*MaxSubjectRunes + 6*MaxExternalIDLen + requestOverhead
	requestOverhead   = 1024
	MaxPerHour        = 60
	MaxQueued         = 200
	rateWindow        = time.Hour
	nameAndScopeShape = `^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`
)

// MaxRequestBytes bounds what the message route reads at all; the per-field caps
// are checked after decoding.
//
// 🔴 IT IS THE WORST-CASE ESCAPED SIZE OF A VALID MESSAGE, NOT A MULTIPLE OF THE
// DECODED CAPS. JSON may spell any byte as a 6-byte `\u00XX` escape — and a
// control character MUST be spelled so; encoding/json also spells `<`, `>` and `&`
// that way — so a valid 16 KiB body can arrive as 96 KiB. A 4-byte rune can arrive
// as a 12-byte surrogate pair, which is the worst per RUNE (sender and subject are
// capped in runes); per BYTE it is the 6× above (body and external_id are capped
// in bytes). requestOverhead (1 KiB) covers the keys, an escaped received_at and
// ordinary indentation. A cap below this refuses a message the per-field caps
// accept (audit round 1).
const MaxRequestBytes = maxRequestBytes

// Status is a message's delivery state. The set is closed (the 0015 CHECK).
type Status string

const (
	// StatusQueued: not yet handed to the chief. A delivery that provably pasted
	// nothing ("pending") leaves the message here.
	StatusQueued Status = "queued"
	// StatusDelivering: the chief named by AgentID is on the turn that hands the
	// message over. 🔴 IT IS WRITTEN BEFORE THE TURN RUNS SO THE CHIEF CAN CITE THE
	// MESSAGE ON THAT VERY TURN — the turn it is told about it, and the one it
	// files a gated task or drafts a reply on. It settles to delivered or
	// unconfirmed after the turn, or back to queued (AgentID cleared) only when the
	// turn provably sent nothing. A process that dies mid-turn leaves it here; the
	// next delivery to the same chief settles it unconfirmed, and it is never
	// sent again ([Deliverer.Deliver]).
	StatusDelivering Status = "delivering"
	// StatusDelivered: the chief's turn completed.
	StatusDelivered Status = "delivered"
	// StatusUnconfirmed: the send failed after the text may have been pasted. It is
	// never sent again.
	StatusUnconfirmed Status = "unconfirmed"
)

// Plugin is one minted plugin's record; the token itself is never stored.
type Plugin struct {
	ID             int64      `json:"id"`
	OwnerID        int64      `json:"ownerId"`
	Name           string     `json:"name"`
	Label          string     `json:"label"`
	DataScope      string     `json:"dataScope"`
	CreatedAt      time.Time  `json:"createdAt"`
	LastSeenAt     *time.Time `json:"lastSeenAt,omitempty"`
	RevokedAt      *time.Time `json:"revokedAt,omitempty"`
	ChiefAgentID   *int64     `json:"-"`
	ChiefSessionID *int64     `json:"-"`
}

// MessageIn is one inbound message as a plugin POSTs it.
type MessageIn struct {
	ExternalID string     `json:"external_id"`
	Sender     string     `json:"sender"`
	Subject    string     `json:"subject"`
	Body       string     `json:"body"`
	ReceivedAt *time.Time `json:"received_at"`
	// NoReply marks a message nobody may answer (a notification, a no-reply
	// sender): no reply draft can be filed for it (migration 0016 refuses one),
	// and the chief's turn says so. Optional; false when absent.
	NoReply bool `json:"no_reply"`
}

// Message is one row of plugin_messages.
type Message struct {
	ID            int64      `json:"id"`
	OwnerID       int64      `json:"-"`
	PluginID      int64      `json:"pluginId"`
	ExternalID    string     `json:"externalId"`
	Sender        string     `json:"sender"`
	Subject       string     `json:"subject"`
	Body          string     `json:"-"`
	ReceivedAt    *time.Time `json:"receivedAt,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	Status        Status     `json:"status"`
	Attempts      int        `json:"attempts"`
	LastAttemptAt *time.Time `json:"lastAttemptAt,omitempty"`
	DeliveredAt   *time.Time `json:"deliveredAt,omitempty"`
	AgentID       *int64     `json:"-"`
	NoReply       bool       `json:"noReply"`
}

// Errors the API maps to status codes.
var (
	// ErrInvalid: the input breaks a stated rule; the message says which (400).
	ErrInvalid = errors.New("plugins: invalid")
	// ErrNotFound: no such row FOR THIS PRINCIPAL (another owner's is the same).
	ErrNotFound = errors.New("plugins: not found")
	// ErrBadToken: unknown, revoked, or its owner is disabled.
	ErrBadToken = errors.New("plugins: unknown or revoked plugin token")
	// ErrRateLimited: the plugin stored MaxPerHour messages in the last hour (429).
	ErrRateLimited = errors.New("plugins: too many messages in the last hour")
	// ErrQueueFull: MaxQueued of the plugin's messages are still undelivered (429).
	ErrQueueFull = errors.New("plugins: too many undelivered messages")
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// HashToken is the stored form of a plugin token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewToken mints TokenPrefix + 43 base64url characters (256 bits).
func NewToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("plugins: mint a token: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// TokenShapeOK reports whether token is shaped like one NewToken mints. A
// FILTER, not authentication: the hash lookup is the property.
func TokenShapeOK(token string) bool {
	rest, ok := strings.CutPrefix(token, TokenPrefix)
	if !ok || len(rest) != 43 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	return err == nil && len(b) == 32
}

// LooksLikeToken reports whether a presented credential is a plugin token by its
// PREFIX alone. It is what every other door refuses on: a malformed `mpl_…` is
// still a plugin credential presented in the wrong place.
func LooksLikeToken(token string) bool { return strings.HasPrefix(token, TokenPrefix) }

var nameRE = regexp.MustCompile(nameAndScopeShape)
var scopeRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$`)

// ValidName checks a plugin's slug: lower-case letters, digits and hyphens,
// 1–40 characters, not starting or ending with a hyphen (the 0015 CHECK).
func ValidName(name string) error {
	if !nameRE.MatchString(name) {
		return invalid("a plugin name is 1-40 lower-case letters, digits and hyphens, not starting or ending with a hyphen")
	}
	return nil
}

// ValidScope checks a data scope (the 0015 CHECK).
func ValidScope(scope string) error {
	if !scopeRE.MatchString(scope) {
		return invalid("a data scope is 1-64 lower-case letters, digits, '.', '_' and '-', starting and ending with a letter or digit")
	}
	return nil
}

// ValidLabel checks a display label: 1–80 runes, no control or invisible
// characters (it is drawn on /plugins and names the chief's session).
func ValidLabel(label string) error {
	if label == "" || utf8.RuneCountInString(label) > MaxLabelRunes || !utf8.ValidString(label) {
		return invalid("a label is 1-%d characters", MaxLabelRunes)
	}
	for _, r := range label {
		if fence.Unsafe(r) {
			return invalid("a label may not carry control or invisible characters")
		}
	}
	return nil
}

// Validate checks an inbound message against every stated cap.
//
// 🔴 EXTERNAL_ID IS AN IDEMPOTENCY KEY, SO IT IS REFUSED RATHER THAN NORMALISED.
// Trimming or folding it would make two distinct upstream ids collide, or one id
// sent twice with different whitespace land twice.
func (in MessageIn) Validate() error {
	switch {
	case in.ExternalID == "" || len(in.ExternalID) > MaxExternalIDLen || !utf8.ValidString(in.ExternalID):
		return invalid("external_id is required: 1-%d bytes of UTF-8", MaxExternalIDLen)
	case strings.TrimSpace(in.Body) == "":
		return invalid("body is required")
	case len(in.Body) > MaxBodyBytes:
		return invalid("body is over %d bytes", MaxBodyBytes)
	case utf8.RuneCountInString(in.Sender) > MaxSenderRunes:
		return invalid("sender is over %d characters", MaxSenderRunes)
	case utf8.RuneCountInString(in.Subject) > MaxSubjectRunes:
		return invalid("subject is over %d characters", MaxSubjectRunes)
	}
	for _, f := range []struct{ name, v string }{{"external_id", in.ExternalID}, {"sender", in.Sender}, {"subject", in.Subject}, {"body", in.Body}} {
		// Postgres refuses invalid UTF-8 and NUL in text; refusing here names the field.
		if !utf8.ValidString(f.v) || strings.ContainsRune(f.v, 0) {
			return invalid("%s must be UTF-8 text without NUL", f.name)
		}
	}
	for _, r := range in.ExternalID {
		if fence.Unsafe(r) || r == ' ' {
			return invalid("external_id may not carry spaces, control or invisible characters")
		}
	}
	return nil
}
