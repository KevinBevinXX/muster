package push

import (
	"context"
	"encoding/json"
	"log"
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
	rep.Devices = len(devs)
	if len(devs) == 0 {
		return rep
	}
	body, err := json.Marshal(p)
	if err != nil {
		s.logger.Printf("push: marshal payload tag=%s: %v", p.Tag, err)
		return rep
	}
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for _, d := range devs {
		wg.Add(1)
		go func(d Device) {
			defer wg.Done()
			res := s.pusher.Send(ctx, d.Sub, body)
			result := s.settle(ctx, d, res)
			metrics.PushDelivery.WithLabelValues(result).Inc()
			mu.Lock()
			defer mu.Unlock()
			switch result {
			case ResultOK:
				rep.Delivered++
			case ResultExpired:
				rep.Expired++
			default:
				rep.Failed++
			}
		}(d)
	}
	wg.Wait()
	s.logger.Printf("push: delivered tag=%s owner=%d to %d/%d device(s) (expired %d, failed %d)",
		p.Tag, owner, rep.Delivered, rep.Devices, rep.Expired, rep.Failed)
	return rep
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
