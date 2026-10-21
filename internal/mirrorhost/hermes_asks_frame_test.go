package mirrorhost

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"math"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestTheLargestWindowListWithHermesAsksFitsOneFrame is the Phase 4 twin of
// TestTheLargestWindowListWithHermesWindowsFitsOneFrame: the windows that
// carry what they wait on (MaxPendingWindows of them) are HERMES windows
// waiting on an approval, every Pending field at its bound — the Hermes ask's
// Kind, Detail, PromptID and Choices included — and every other window a
// Hermes window too. An invariant guard over the constants, with the same
// control: at the measured size per window, twice the count must not fit.
func TestTheLargestWindowListWithHermesAsksFitsOneFrame(t *testing.T) {
	worst := func(n int) []WindowInfo {
		out := make([]WindowInfo, 0, n)
		for i := 0; i < n; i++ {
			name := strings.Repeat("<", 128-len(itoa(i))) + itoa(i)
			id := itoa(i) + "h"
			w := WindowInfo{Session: name, ID: "@" + strings.Repeat("9", 10), Index: 1 << 20, Active: true,
				Pane: "%1", PanePID: 1, Activity: 1791903960, Command: strings.Repeat(">", mirror.MaxCommand),
				Others: otherClaudeAtBound(i),
				Agent: &AgentPane{Runtime: mirror.RuntimeHermes, Status: mirror.StatusWaiting, StatusSince: math.MaxInt64,
					Gen: id + strings.Repeat("a", 128-len(id))}}
			if i < mirror.MaxPendingWindows {
				w.Agent.Pending = &mirror.Pending{Kind: mirror.PendingApproval, Summary: strings.Repeat("<", mirror.MaxPendingSummary),
					Tool: strings.Repeat("&", mirror.MaxPendingTool), Last: strings.Repeat(">", mirror.MaxPendingLast),
					Detail: strings.Repeat("<", mirror.MaxPendingDetail), PromptID: strings.Repeat("p", 64), Choices: "once deny"}
			}
			out = append(out, w)
		}
		return out
	}
	codename := strings.Repeat("&", mirror.MaxLabel-len(":1048576"))
	raw := worst(mirror.MaxWindows)
	codenames := map[string]string{}
	for _, w := range raw {
		codenames[w.Session] = codename
	}
	h := &hostSide{c: &Client{cfg: Config{Backend: &windowsBackend{windows: raw}, Codenames: codenames}, log: log.New(io.Discard, "", 0)},
		ctx: context.Background(), version: mirror.ProtocolVersion}
	l, ok := h.read()
	if !ok {
		t.Fatal("the read failed")
	}
	asks := 0
	for _, w := range l.windows {
		if p := w.Pending; p != nil {
			if len(p.Detail) != mirror.MaxPendingDetail || len(p.PromptID) != 64 || p.Choices != "once deny" || len(p.Summary) != mirror.MaxPendingSummary {
				t.Fatalf("instrument check: a worst-case Pending field was dropped: %+v", p)
			}
			asks++
		}
	}
	if asks != mirror.MaxPendingWindows || len(l.windows) != mirror.MaxWindows {
		t.Fatalf("instrument check: %d asks in %d windows", asks, len(l.windows))
	}
	m := l.msg(mirror.TypeHello)
	m.Version, m.Host, m.Interactive, m.PanePaste, m.StatusReport, m.HermesSend, m.HermesAnswer = mirror.ProtocolVersion, strings.Repeat("h", 63), true, true, true, true, true
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	got := len(b)
	if got > mirror.MaxFrame {
		t.Fatalf("the largest window list with Hermes asks encodes to %d bytes, over MaxFrame (%d)", got, mirror.MaxFrame)
	}
	if perWindow := got / mirror.MaxWindows; 2*mirror.MaxWindows*perWindow <= mirror.MaxFrame {
		t.Fatalf("instrument check: at %d bytes a window, even twice MaxWindows fits — this test could not see an overrun", perWindow)
	}
	t.Logf("largest list with Hermes asks: %d windows in %d bytes (%.1f%% of MaxFrame)", mirror.MaxWindows, got, 100*float64(got)/float64(mirror.MaxFrame))
}
