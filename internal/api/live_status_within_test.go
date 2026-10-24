package api

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/provision"
)

// answeringProv answers Instances at once with one instance.
type answeringProv struct{ stubProvisioner }

func (answeringProv) Instances(context.Context) ([]provision.Instance, error) {
	return []provision.Instance{{Ref: provision.Ref{Name: "a"}}}, nil
}

// TestABoundedInstanceReadThatAnswersIsNeverDroppedAsATimeout: a provisioner
// that answers at once, read with an hour's bound, is observed every time —
// never a nil index ("not observed") and never the "no answer within" line.
//
// The bug this guards: the read's goroutine cancelled ctx right after sending
// its answer, so a caller that reached the select late found both cases ready
// and dropped the answer at random. That made the stopped-agent reaper destroy
// nothing under CI load (TestTheReaperIsOffAtZero,
// TestAStoppedRowOverALiveInstanceIsNotReaped). Whether a given call loses
// depends on scheduling, so the read is repeated from many goroutines at once;
// on the old code this loses within the first few hundred calls.
func TestABoundedInstanceReadThatAnswersIsNeverDroppedAsATimeout(t *testing.T) {
	var buf lockedBuf
	s := &Server{logger: log.New(&buf, "", 0), ext: Extensions{Provisioner: answeringProv{}}}
	const workers, each = 16, 2000
	var wg sync.WaitGroup
	var mu sync.Mutex
	dropped := 0
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if idx := s.instanceIndexWithin(context.Background(), time.Hour); idx == nil || idx["a"] == nil {
					mu.Lock()
					dropped++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if dropped != 0 {
		t.Fatalf("%d of %d answered reads came back as not observed; log:\n%s", dropped, workers*each, firstLines(buf.String(), 3))
	}
	if bytes.Contains([]byte(buf.String()), []byte("no answer within")) {
		t.Fatalf("an answered read was logged as a timeout:\n%s", firstLines(buf.String(), 3))
	}
}

// stuckProv's Instances never answers until release is closed, ignoring ctx,
// so the only way out of a bounded read is the read's own ctx ending.
type stuckProv struct {
	stubProvisioner
	release chan struct{}
}

func (p stuckProv) Instances(context.Context) ([]provision.Instance, error) {
	<-p.release
	return nil, nil
}

// TestABoundedInstanceReadNamesWhyItGaveUp: a read whose caller's ctx is
// cancelled is logged as cancelled, not as "no answer within" the bound (an
// hour, nowhere near reached). CONTROL: past a 10ms bound, the same stuck read
// is logged as "no answer within 10ms". Both answer nil (not observed).
func TestABoundedInstanceReadNamesWhyItGaveUp(t *testing.T) {
	for name, tc := range map[string]struct {
		within        time.Duration
		cancel        bool
		want, notWant string
	}{
		"caller cancelled":        {time.Hour, true, "instance read cancelled (context canceled)", "no answer within"},
		"control: past the bound": {10 * time.Millisecond, false, "no answer within 10ms", "instance read cancelled"},
	} {
		t.Run(name, func(t *testing.T) {
			var buf lockedBuf
			p := stuckProv{release: make(chan struct{})}
			t.Cleanup(func() { close(p.release) })
			s := &Server{logger: log.New(&buf, "", 0), ext: Extensions{Provisioner: p}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			if idx := s.instanceIndexWithin(ctx, tc.within); idx != nil {
				t.Fatalf("a read that never answered returned an index: %v", idx)
			}
			logs := buf.String()
			if !strings.Contains(logs, tc.want) || strings.Contains(logs, tc.notWant) {
				t.Fatalf("want %q and not %q in the log:\n%s", tc.want, tc.notWant, logs)
			}
		})
	}
}

func firstLines(s string, n int) string {
	lines := bytes.SplitN([]byte(s), []byte("\n"), n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return string(bytes.Join(lines, []byte("\n")))
}
