package push

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"

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
// 🔴 IDS AND A SHORT TITLE, NOTHING ELSE. Title is the browser payload's own
// WHO line (every notification helper writes WHO and WHERE, never WHAT;
// docs/attention.md), cut to MaxNativeTitle characters. The browser's Body
// line is NOT carried, and URL is where a tap opens, a path on muster. The app
// fetches anything more after delivery, with its device credential.
//
// 🔴 AND IT MUST FIT IN 4096 ENCRYPTED BYTES. A self-hosted ntfy accepts a
// larger `up*` message with HTTP 200 and silently truncates it to 4096 bytes,
// which the phone can then not decrypt (measured in the Phase 0 spikes,
// ntfy 2.31). SendTo refuses a native payload over NativeMaxPlaintext
// (nativeBody), loudly; see that constant for the arithmetic.
//
// ⚠ PHASE 1 CARRIES NO `actions`, `conv` OR `state`. An action id is a
// promise that a signed /api/device/actions/* route exists (Phase 2, with
// the route ledger that pins that promise), and the state delta feeds the
// widgets (Phase 4). Sending either before its route or consumer exists would
// be a field no code reads.
type PayloadV2 struct {
	V     int    `json:"v"`
	Type  string `json:"type"`
	ID    string `json:"id,omitempty"`
	Tag   string `json:"tag,omitempty"`
	Title string `json:"title,omitempty"`
	URL   string `json:"url,omitempty"`
}

// MaxNativeTitle is the longest title payload v2 carries, in characters.
const MaxNativeTitle = 100

// The encrypted size of one native push, by RFC 8188 (aes128gcm) as webpush-go
// builds it: ONE record of nativeRecordSize bytes — the header (salt, record
// size, key-id length, the sender's P-256 public key), then the plaintext, its
// padding delimiter and zero padding up to the record, then the GCM tag.
// webpush-go pads every message to the full record, so EVERY native body is
// exactly nativeRecordSize bytes and a plaintext over NativeMaxPlaintext cannot
// be encrypted at all (TestTheEncryptedNativeBodyIsAtMost4096Bytes measures it).
const (
	// ntfyMaxBody is where ntfy truncates an up* message (Phase 0 spike).
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
// encrypted body.
var ErrNativeTooLarge = fmt.Errorf("push: a native payload is larger than %d bytes, so its encrypted body would exceed ntfy's %d bytes", NativeMaxPlaintext, ntfyMaxBody)

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
	v.Title, v.URL = p.Title, p.Data["url"]
	if r := []rune(v.Title); len(r) > MaxNativeTitle {
		v.Title = string(r[:MaxNativeTitle-1]) + "…"
	}
	return v
}

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
