package mirrorhost

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/hermeshost"
	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
)

// TestUserRowAfterComparesStrippedAndOnlyNewRows: the database confirm finds
// a user row PAST the cursor, in the conversation's lineage, whose content
// stripped is the message stripped (Hermes submits raw_text.strip(); a
// collapsed paste comes back unstripped) — and nothing else: an older row
// with the same text, another text, an assistant row with the text.
func TestUserRowAfterComparesStrippedAndOnlyNewRows(t *testing.T) {
	h := hermestest.New(t)
	root := hermestest.LocalID()
	h.Session(root, hermestest.SessionOpts{})
	before := h.Message(root, hermestest.Msg{Role: "user", Content: "hello hermes"})
	state := hermeshost.NewHome(h.Dir).OpenState()
	t.Cleanup(func() { _ = state.Close() })
	ctx := context.Background()
	check := func(want string, ok bool) {
		t.Helper()
		got, err := userRowAfter(ctx, state, root, before, want)
		if err != nil || got != ok {
			t.Fatalf("userRowAfter(%q) = %t, %v; want %t", want, got, err, ok)
		}
	}
	check("hello hermes", false) // only the row AT the cursor holds it
	h.Message(root, hermestest.Msg{Role: "assistant", Content: "hello hermes"})
	check("hello hermes", false) // not a user row
	h.Message(root, hermestest.Msg{Role: "user", Content: "\n  hello hermes  \n"})
	check(" hello hermes\n", true) // both sides stripped
	check("hello  hermes", false)
	check("hello", false)
}

// TestMultiplexerInTreeLooksBelowHermes: the gated send's multiplexer check
// walks PAST a Hermes process (InTree stops at it), so a multiplexer client
// anywhere in the pane's tree is found; a tree without one is not.
func TestMultiplexerInTreeLooksBelowHermes(t *testing.T) {
	proc := newFakeProc(t)
	proc.add(100, 1, "zsh", "/w", "-zsh")
	proc.add(110, 100, "hermes", "/w", "python3.14", "/x/bin/hermes", "chat")
	proc.add(120, 110, "tmux: client", "/w", "tmux", "-L", "other", "attach")
	proc.add(200, 1, "zsh", "/w", "-zsh")
	proc.add(210, 200, "hermes", "/w", "python3.14", "/x/bin/hermes", "chat")
	id := HermesIdentity{Proc: proc.root}
	if hp, found, err := id.InTree(100); err != nil || !found || hp.PID != 110 {
		t.Fatalf("instrument check: InTree(100) = %+v %t %v; want the Hermes process first", hp, found, err)
	}
	if hp, found, err := id.MultiplexerInTree(100); err != nil || !found || hp.PID != 120 {
		t.Fatalf("MultiplexerInTree(100) = %+v %t %v; want the tmux client under Hermes", hp, found, err)
	}
	if hp, found, err := id.MultiplexerInTree(200); err != nil || found {
		t.Fatalf("MultiplexerInTree(200) = %+v %t %v; want none", hp, found, err)
	}
}
