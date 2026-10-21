package push

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"unicode"

	"github.com/ZacxDev/muster/internal/metrics"
)

// Payload is the JSON the service worker's `push` handler reads
// (web/static/sw.js): type "resolved" closes notifications by tag and shows
// nothing; any other type shows Title/Body and opens Data["url"] on a tap.
type Payload struct {
	Type  string            `json:"type"`
	ID    string            `json:"id,omitempty"`
	Title string            `json:"title,omitempty"`
	Body  string            `json:"body,omitempty"`
	Tag   string            `json:"tag,omitempty"`
	Data  map[string]string `json:"data,omitempty"`
}

// PayloadV2 is what a NATIVE device (KindAndroid) receives in place of
// Payload: the Android app's payload (docs/design/android.md, "Payload v2").
//
// 🔴 IDS AND WHO/WHERE, NEVER WHAT. Title and Body are the browser payload's
// own lines, which every notification helper writes as WHO and WHERE, never
// WHAT (docs/attention.md); v2 adds nothing to them and cuts each, on a
// character-cluster boundary, to MaxNativeTitle and MaxNativeBody. URL is
// where a tap opens, a path on muster. The app fetches anything more after
// delivery, with its device credential.
//
// ⚠ PHASE 1 CARRIES NO `actions`, `conv` OR `state`. An action id is a
// promise that a signed /api/device/actions/* route exists (Phase 2, with the
// route ledger that pins it, and the app's wire ids: docs/design/android.md,
// "Action ids"), and the state delta feeds the widgets (Phase 4). Sending
// either before its route or consumer exists would be a field no code reads.
type PayloadV2 struct {
	V     int    `json:"v"`
	Type  string `json:"type"`
	ID    string `json:"id,omitempty"`
	Tag   string `json:"tag,omitempty"`
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
	URL   string `json:"url,omitempty"`
}

// MaxNativeTitle and MaxNativeBody are the longest title and body payload v2
// carries, in characters (runes). Even with every character JSON-escaped to
// six bytes they total 1800 bytes, well inside NativeMaxPlaintext.
const (
	MaxNativeTitle = 100
	MaxNativeBody  = 200
)

// The size of one native push, by RFC 8188 (aes128gcm) as webpush-go builds
// it: ONE record of nativeRecordSize bytes — the header (salt, record size,
// key-id length, the sender's P-256 public key), then the plaintext, its
// padding delimiter and zero padding up to the record, then the GCM tag.
// webpush-go pads every message to the full record, so EVERY native body is
// exactly nativeRecordSize bytes, and it refuses a plaintext over
// NativeMaxPlaintext itself (webpush.ErrMaxPadExceeded):
// TestTheEncryptedNativeBodyIsAtMost4096Bytes measures both.
//
// ⚠ SO NTFY'S TRUNCATION CANNOT HAPPEN TO A PUSH FROM HERE. ntfy accepts an
// `up*` message over its message-size-limit (4096 bytes by default) with 200
// and silently truncates it (Phase 0, S9), which matters for a sender that
// does not pad to one record; with the record pinned at 4096, muster's bodies
// never exceed it. What the cap below adds is the ANSWER: an over-budget
// payload is refused BEFORE the pusher, named `oversize` in the log and the
// counter, and does not count against the device's fail_count as the
// library's encryption error would.
const (
	// ntfyMaxBody is ntfy's default message-size-limit (Phase 0, S9).
	ntfyMaxBody = 4096
	// nativeRecordSize is the aes128gcm record size the pusher asks for. It is
	// pinned (WebpushPusher.Send passes it) rather than left to the library's
	// default, so a dependency upgrade cannot move it past ntfy's limit.
	nativeRecordSize   = ntfyMaxBody
	aes128gcmHeaderLen = 16 + 4 + 1 + 65 // salt, rs, idlen, keyid
	aes128gcmTagLen    = 16
	paddingDelimiter   = 1
	// NativeMaxPlaintext is the largest payload v2, in bytes, that encrypts
	// into nativeRecordSize: 4096 - 86 - 16 - 1 = 3993.
	NativeMaxPlaintext = nativeRecordSize - aes128gcmHeaderLen - aes128gcmTagLen - paddingDelimiter
)

// ErrNativeTooLarge refuses a payload v2 that would not fit in one 4096-byte
// record.
var ErrNativeTooLarge = fmt.Errorf("push: a native payload is larger than %d bytes, so it does not fit in one %d-byte record", NativeMaxPlaintext, nativeRecordSize)

// nativeBody is p's payload v2 as sent, or ErrNativeTooLarge.
func nativeBody(p Payload) ([]byte, error) {
	b, err := json.Marshal(p.V2())
	if err != nil {
		return nil, err
	}
	if len(b) > NativeMaxPlaintext {
		return nil, fmt.Errorf("%w (%d bytes)", ErrNativeTooLarge, len(b))
	}
	return b, nil
}

// V2 is p as a native device receives it. A "resolved" carries only its type,
// id and tag: it shows nothing.
func (p Payload) V2() PayloadV2 {
	v := PayloadV2{V: 2, Type: p.Type, ID: p.ID, Tag: p.Tag}
	if p.Type == TypeResolved {
		return v
	}
	v.Title, v.Body, v.URL = cutClusters(p.Title, MaxNativeTitle), cutClusters(p.Body, MaxNativeBody), p.Data["url"]
	return v
}

// cutClusters cuts s to at most n runes, ending with "…" when it cut, without
// splitting what a reader sees as one character: it backs off over combining
// marks, variation selectors, skin-tone modifiers, emoji tag characters, a
// zero-width joiner on either side of the cut, and half a regional-indicator
// flag. A cheap approximation of Unicode grapheme clusters (no tables), for a
// notification line, not a text engine.
func cutClusters(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := n - 1 // room for the ellipsis
	for cut > 0 && (extends(r[cut]) || r[cut-1] == zwj) {
		cut--
	}
	if cut > 0 && isRegional(r[cut]) && isRegional(r[cut-1]) {
		ri := 0
		for i := cut - 1; i >= 0 && isRegional(r[i]); i-- {
			ri++
		}
		if ri%2 == 1 { // the rune at cut would pair with the one before it
			cut--
		}
	}
	return string(r[:cut]) + "…"
}

const zwj = '\u200d'

// extends reports whether r attaches to the character before it.
func extends(r rune) bool {
	return unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == zwj ||
		(r >= 0xFE00 && r <= 0xFE0F) || (r >= 0x1F3FB && r <= 0x1F3FF) || (r >= 0xE0020 && r <= 0xE007F)
}

func isRegional(r rune) bool { return r >= 0x1F1E6 && r <= 0x1F1FF }

// Report is what one SendTo did. Devices is how many the owner had.
type Report struct {
	Devices   int
	Delivered int
	Expired   int
	Failed    int
}

// Delivery results, the closed label set of muster_push_delivery_total.
const (
	ResultOK      = "ok"
	ResultExpired = "expired"
	ResultFailed  = "failed"
	// ResultDisallowed is a device whose endpoint is no longer on an allowed
	// push service: nothing is sent, and the row is pruned.
	ResultDisallowed = "disallowed"
	// ResultOversize is a native push refused before sending: its payload v2
	// would not fit in one 4096-byte encrypted body (ErrNativeTooLarge).
	ResultOversize = "oversize"
)

// Service sends one owner's notifications to that owner's devices.
type Service struct {
	store  Store
	pusher Pusher
	vapid  VAPIDConfig
	logger *log.Logger
}

// NewService builds the Service. logger may be nil.
func NewService(store Store, pusher Pusher, vapid VAPIDConfig, logger *log.Logger) *Service {
	if logger == nil {
		logger = log.Default()
	}
	return &Service{store: store, pusher: pusher, vapid: vapid, logger: logger}
}

// PublicKey is the VAPID public key browsers subscribe with.
func (s *Service) PublicKey() string { return s.vapid.PublicKey }

// Store is the subscription store (the HTTP handlers' reads and writes).
func (s *Service) Store() Store { return s.store }

// Allowlist is the store's allowlist: the one source for what may be stored
// and what may be sent to.
func (s *Service) Allowlist() Allowlist { return s.store.Allowlist() }

// allowedNow reports whether d's endpoint is still on an allowed push service
// for its kind, judged NOW rather than when it was stored.
//
// 🔴 THE ALLOWLIST IS RE-CHECKED AT SEND TIME, NOT ONLY AT CLAIM. A host taken
// out of MUSTER_PUSH_EXTRA_HOSTS (or a row stored before a rule tightened)
// would otherwise keep receiving a POST per notification, forever.
func (s *Service) allowedNow(d Device) bool {
	if d.Kind == KindAndroid {
		return s.store.Allowlist().Allows(d.Sub.Endpoint)
	}
	return s.store.Allowlist().AllowsBrowser(d.Sub.Endpoint)
}

// dropDisallowed prunes a device whose endpoint is no longer allowed, sending
// it nothing, and names the result.
func (s *Service) dropDisallowed(ctx context.Context, d Device) string {
	ep := d.Sub.Endpoint
	if _, err := s.store.Prune(ctx, d); err != nil {
		s.logger.Printf("push: prune disallowed %s (owner=%d) failed: %s", Short(ep), d.OwnerID, Redact(err, ep))
	} else {
		s.logger.Printf("push: pruned %s (owner=%d): its push service is no longer allowed; nothing was sent", Short(ep), d.OwnerID)
	}
	return ResultDisallowed
}

// Probe sends payload to sub, a subscription that is NOT stored: the test
// push a paired device's registration must pass before it is kept (as
// UnifiedPush recommends). It settles nothing and touches no row; the caller
// reads the result.
func (s *Service) Probe(ctx context.Context, sub Subscription, payload []byte) SendResult {
	return s.pusher.Send(ctx, sub, payload)
}

// SendTo delivers p to every device OWNER holds — and to nobody else's — in
// parallel, pruning devices the push service reports gone (404/410) and
// stamping last_ok_at / fail_count on the rest. It never returns an error: a
// notification is best-effort at every call site, so every failure is LOGGED
// (redacted) and COUNTED (muster_push_delivery_total) instead, which is what
// keeps a dead pipeline from looking like a quiet one.
func (s *Service) SendTo(ctx context.Context, owner int64, p Payload) Report {
	var rep Report
	devs, err := s.store.ForOwner(ctx, owner)
	if err != nil {
		s.logger.Printf("push: list devices for owner=%d: %v", owner, err)
		return rep
	}
	if p.Type == TypeResolved {
		devs = withoutSilentPenalty(devs)
	}
	rep.Devices = len(devs)
	if len(devs) == 0 {
		return rep
	}
	body, err := json.Marshal(p)
	if err != nil {
		s.logger.Printf("push: marshal payload tag=%s: %v", p.Tag, err)
		return rep
	}
	// A native device gets payload v2; a browser the payload its service
	// worker reads. Keyed on the subscription's recorded kind, never guessed
	// from the endpoint.
	native, nativeErr := nativeBody(p)
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for _, d := range devs {
		wg.Add(1)
		go func(d Device) {
			defer wg.Done()
			b := body
			if d.Kind == KindAndroid {
				b = native
			}
			var result string
			if !s.allowedNow(d) {
				result = s.dropDisallowed(ctx, d)
			} else if d.Kind == KindAndroid && nativeErr != nil {
				// Refused, never sent: ntfy would accept it and truncate it into
				// something the phone cannot decrypt.
				s.logger.Printf("push: REFUSED tag=%s to %s (owner=%d): %v", p.Tag, Short(d.Sub.Endpoint), d.OwnerID, nativeErr)
				result = ResultOversize
			} else {
				result = s.settle(ctx, d, s.pusher.Send(ctx, d.Sub, b))
			}
			metrics.PushDelivery.WithLabelValues(result).Inc()
			mu.Lock()
			defer mu.Unlock()
			switch result {
			case ResultOK:
				rep.Delivered++
			case ResultExpired, ResultDisallowed:
				rep.Expired++
			default: // failed, oversize
				rep.Failed++
			}
		}(d)
	}
	wg.Wait()
	s.logger.Printf("push: delivered tag=%s owner=%d to %d/%d device(s) (expired %d, failed %d)",
		p.Tag, owner, rep.Delivered, rep.Devices, rep.Expired, rep.Failed)
	return rep
}

// withoutSilentPenalty drops Safari (web.push.apple.com) BROWSER devices from
// a "resolved" send. A native device (KindAndroid) always gets it: the app
// withdraws the notification by tag and shows nothing, and no distributor
// penalises a silent push — which is the clear Safari subscriptions are
// spared. Keyed on the recorded kind, not on the host.
//
// 🔴 A resolved MESSAGE SHOWS NOTHING, AND SAFARI PENALISES A PUSH THAT SHOWS
// NOTHING. WebKit's push contract is "every push displays a notification"; a
// silent one counts against the subscription and repeated ones can get it
// revoked. Dropping the clear on Apple devices costs a stale notification on
// an iPhone; sending it risks losing the device's notifications altogether.
func withoutSilentPenalty(devs []Device) []Device {
	out := devs[:0:0]
	for _, d := range devs {
		if d.Kind == KindAndroid {
			out = append(out, d)
			continue
		}
		if u, err := url.Parse(d.Sub.Endpoint); err == nil && strings.EqualFold(u.Hostname(), applePushHost) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// settle records one attempt's outcome against its device and names the
// result.
func (s *Service) settle(ctx context.Context, d Device, res SendResult) string {
	ep := d.Sub.Endpoint
	switch {
	case res.Err != nil:
		s.logger.Printf("push: send to %s (owner=%d) failed: %s", Short(ep), d.OwnerID, Redact(res.Err, ep))
		if err := s.store.MarkFailed(ctx, d); err != nil {
			s.logger.Printf("push: record failure for %s: %s", Short(ep), Redact(err, ep))
		}
		return ResultFailed
	case res.Expired:
		if _, err := s.store.Prune(ctx, d); err != nil {
			s.logger.Printf("push: prune %s (owner=%d) failed: %s", Short(ep), d.OwnerID, Redact(err, ep))
		} else {
			s.logger.Printf("push: pruned expired subscription %s (owner=%d, status %d)", Short(ep), d.OwnerID, res.StatusCode)
		}
		return ResultExpired
	case res.StatusCode >= 200 && res.StatusCode < 300:
		if err := s.store.MarkDelivered(ctx, d); err != nil {
			s.logger.Printf("push: record delivery for %s: %s", Short(ep), Redact(err, ep))
		}
		return ResultOK
	default:
		s.logger.Printf("push: send to %s (owner=%d) returned status %d", Short(ep), d.OwnerID, res.StatusCode)
		if err := s.store.MarkFailed(ctx, d); err != nil {
			s.logger.Printf("push: record failure for %s: %s", Short(ep), Redact(err, ep))
		}
		return ResultFailed
	}
}
