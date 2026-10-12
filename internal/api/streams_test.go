package api

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestEveryEventStreamHandlerEndsOnShutdown is a LEDGER over the functions in
// package api whose body sets the literal "text/event-stream": each must take
// its loop's context from streamContext, or it holds http.Server.Shutdown open
// for the whole drain budget (see EndStreams). It fails when the set GROWS — a
// new stream handler must be added here deliberately — or SHRINKS, and when any
// member does not call streamContext.
//
// ⚠ THAT IS ITS WHOLE REACH, AND IT IS NARROWER THAN "EVERY STREAM THIS SERVER
// CAN HOLD OPEN". It reads one package for one literal. A stream relayed through
// the /connect/ reverse proxy (internal/connectors) sets no such literal and is
// invisible here; it ends through connectors.Proxy.StreamsEnd, which this ledger
// does not read — cmd/muster-server's TestShutdownEndsAProxiedEventStreamOnBothListeners
// is what pins it.
//
// It is structural. The behavioural proof for /events is cmd/muster-server's
// TestShutdownDoesNotWaitOutAnOpenEventStream; the agent log stream needs a
// provisioner to reach and is covered by this ledger only.
func TestEveryEventStreamHandlerEndsOnShutdown(t *testing.T) {
	want := []string{"handleAgentLogsStream", "handleEvents"}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var got, missing []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			body := string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
			if !strings.Contains(body, `"text/event-stream"`) {
				continue
			}
			got = append(got, fn.Name.Name)
			if !strings.Contains(body, "s.streamContext(") {
				missing = append(missing, fn.Name.Name)
			}
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("package api functions setting the literal text/event-stream = %v, want %v — a new stream handler must "+
			"end on shutdown (take its context from s.streamContext) and be added here", got, want)
	}
	if len(missing) > 0 {
		t.Errorf("stream handlers not ending on shutdown (no s.streamContext): %v", missing)
	}
}

// TestStreamContextEndsOnEndStreamsAndNotBefore pins streamContext's two
// halves: it is NOT cancelled while the server runs, and it IS cancelled by
// EndStreams — including for a stream opened after EndStreams ran.
func TestStreamContextEndsOnEndStreamsAndNotBefore(t *testing.T) {
	s := &Server{}
	ctx, cancel := s.streamContext(context.Background())
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatal("a stream context was cancelled before EndStreams")
	case <-time.After(50 * time.Millisecond):
	}
	s.EndStreams()
	s.EndStreams() // idempotent
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("EndStreams did not cancel an open stream's context")
	}
	late, lateCancel := s.streamContext(context.Background())
	defer lateCancel()
	select {
	case <-late.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a stream opened after EndStreams was not ended")
	}
}
