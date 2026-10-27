package api

import (
	"context"
	"log"
	"os"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
)

// answersStore is an attention.Store whose AnswersForTask returns a fixed set,
// WHATEVER the task — so ownerAnswer's own checks are what decide, with no
// query or row-level security in front of them.
type answersStore struct {
	attention.Store
	asks []attention.Ask
}

func (s answersStore) AnswersForTask(context.Context, int64) ([]attention.Ask, error) {
	return s.asks, nil
}

// TestOwnerAnswerChecksTaskOwnerAndWordsItself: ownerAnswer does not trust its
// store to have filtered. Owner 7, task 41, words W: an ask of another task,
// one of another owner, and one with other words are each refused even when
// the store hands them over; the matching one is found.
func TestOwnerAnswerChecksTaskOwnerAndWordsItself(t *testing.T) {
	const owner, task = int64(7), int64(41)
	words := "open with the example-blocks skill"
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(os.Stderr, "", 0))
	for _, c := range []struct {
		name string
		ask  attention.Ask
		ok   bool
	}{
		{"match", attention.Ask{ID: 1, Owner: owner, TaskID: task, Answer: words}, true},
		{"match after trim and CRLF", attention.Ask{ID: 2, Owner: owner, TaskID: task, Answer: "\r\n" + words + "  "}, true},
		{"another task", attention.Ask{ID: 3, Owner: owner, TaskID: task + 1, Answer: words}, false},
		{"another owner", attention.Ask{ID: 4, Owner: owner + 1, TaskID: task, Answer: words}, false},
		{"one more word", attention.Ask{ID: 5, Owner: owner, TaskID: task, Answer: words + " now"}, false},
		{"case", attention.Ask{ID: 6, Owner: owner, TaskID: task, Answer: "Open with the example-blocks skill"}, false},
	} {
		s.ext.Attention = answersStore{asks: []attention.Ask{c.ask}}
		got, why := s.ownerAnswer(context.Background(), owner, task, words)
		if c.ok != (why == "") || (c.ok && got.ID != c.ask.ID) {
			t.Errorf("%s: got ask %d, refusal %q; want ok=%v", c.name, got.ID, why, c.ok)
		}
		if !c.ok && why != taskDispatchInstructionsUnsourced {
			t.Errorf("%s: refusal %q", c.name, why)
		}
	}
}
