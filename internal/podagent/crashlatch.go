package podagent

import "time"

// The crash latch and restart backoff ccd and hermesd share.
//
// 🔴 A CRASH LOOP IS A POD PROBLEM, AND IS HANDED TO KUBERNETES. Each restart
// waits backoffMin·2^(k-1) (capped at backoffMax), k being the number of exits
// within window. When maxExits exits fall within window the supervisor stops
// restarting and latches; its /healthz then answers non-200, so the liveness
// probe restarts the pod and the restart count is visible in `kubectl get pod`.

// RecordExit counts one exit at now against the exits already within window
// and decides what follows it: the delay before the next start, or (latched)
// no next start at all. It returns the exits still within the window,
// including this one.
func RecordExit(exits []time.Time, now time.Time, window time.Duration, maxExits int,
	backoffMin, backoffMax time.Duration) (kept []time.Time, delay time.Duration, latched bool) {
	kept = exits[:0]
	for _, t := range exits {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	k := len(kept)
	if k >= maxExits {
		return kept, 0, true
	}
	delay = backoffMin
	for i := 1; i < k && delay < backoffMax; i++ {
		delay *= 2
	}
	return kept, min(delay, backoffMax), false
}
