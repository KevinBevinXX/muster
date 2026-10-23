package askdrive

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/askq"
)

// CONTINUING MUSTER'S OWN PARTIAL ANSWER. The picker is askdrive's FakePicker,
// which draws every three-question state as Claude Code 2.1.289 does
// (TestTheFakeDrawsWhatTheCLIDraws pins it to the real captures).
//
// The operator's report: a multi-question answer "gets stuck mid answer with
// 'someone else started answering'". The shape: a drive types question 1, is
// lost at question 2 (a key whose screen never came), and leaves question 1
// answered in the picker; the retry planned from the untouched picker, found
// question 1 answered, and refused before its first key as someone answering
// in the terminal — muster's own keys, mislabelled, and the answer could never
// be finished from muster.

// lostAfterQ1 is the three-question picker after a drive that answered
// question 1 (its own words: "4", the text, Enter) and lost the first key of
// question 2 — the key never reached the picker, so the drive timed out on
// its screen. It returns the pane and the keys the drive reported sending.
func lostAfterQ1(t *testing.T) (*FakePicker, int) {
	t.Helper()
	p := NewFakePicker(three)
	lp := &lookPane{fakePane: &fakePane{p: p}, drop: func(n int) bool { return n == 3 }}
	keys, err := DriveFrom(context.Background(), lp, three, threeChoices, nil, fast)
	if !errors.Is(err, ErrLost) || !strings.Contains(err.Error(), "after key 4") {
		t.Fatalf("drive 1: %v (keys %q)", err, lp.sent)
	}
	if keys != 4 {
		t.Fatalf("drive 1 reported %d keys sent, sent %q", keys, lp.sent)
	}
	if err := Match(Parse(p.Screen()), Want{Kind: Question, Q: 1, Cursor: 1}, three, nil); err != nil {
		t.Fatalf("drive 1 did not leave question 1 answered and question 2 untouched: %v\n%s", err, p.Screen())
	}
	return p, keys
}

// TestARetryContinuesMustersOwnPartialAnswer: handed what drive 1 sent, the
// retry continues from question 2 and the CLI takes the whole answer.
func TestARetryContinuesMustersOwnPartialAnswer(t *testing.T) {
	p, keys := lostAfterQ1(t)
	fp := &fakePane{p: p}
	got, err := DriveFrom(context.Background(), fp, three, threeChoices, &Progress{Choices: threeChoices, Keys: keys}, fast)
	if err != nil {
		t.Fatalf("the retry: %v (keys %q)\n%s", err, fp.sent, p.Screen())
	}
	answers, cancelled, done := p.Result()
	if !done || cancelled || !reflect.DeepEqual(answers, Answers(three, threeChoices)) {
		t.Fatalf("submitted %v (cancelled %v done %v)", answers, cancelled, done)
	}
	steps, _ := Plan(three, threeChoices)
	if got != len(steps) {
		t.Fatalf("reported %d keys, the plan has %d", got, len(steps))
	}
	// It typed only what was left: question 1's three keys were not typed
	// again (its own words would have gone into question 2's rows).
	if len(fp.sent) != len(steps)-3 || fp.sent[0] != "2" {
		t.Fatalf("the retry typed %q", fp.sent)
	}
}

// TestARetryWithoutTheRecordIsRefused: the same picker, and a drive that is
// NOT handed what was typed (an older host, a restarted muster): question 1
// answered is not muster's to vouch for, so nothing is typed — as before.
func TestARetryWithoutTheRecordIsRefused(t *testing.T) {
	p, _ := lostAfterQ1(t)
	fp := &fakePane{p: p}
	if _, err := DriveFrom(context.Background(), fp, three, threeChoices, nil, fast); !errors.Is(err, ErrNotUntouched) || len(fp.sent) != 0 {
		t.Fatalf("no record: %v, sent %q", err, fp.sent)
	}
}

// TestARetryRefusesSomeoneElsesKeys: muster typed question 1, then someone
// ticked a box in question 2 in the terminal. That is no state muster's keys
// led to: refused as someone answering it, nothing typed.
func TestARetryRefusesSomeoneElsesKeys(t *testing.T) {
	p, keys := lostAfterQ1(t)
	p.Type("3")
	fp := &fakePane{p: p}
	_, err := DriveFrom(context.Background(), fp, three, threeChoices, &Progress{Choices: threeChoices, Keys: keys}, fast)
	if !errors.Is(err, ErrNotUntouched) || len(fp.sent) != 0 {
		t.Fatalf("someone else's keys after muster's: %v, sent %q", err, fp.sent)
	}
}

// TestARetryNeverVouchesForMoreThanMusterTyped: muster typed question 1's
// answer only (one key); someone then answered question 2 in the terminal,
// leaving the picker on question 3 — a screen muster's OWN plan passes
// through, but past the keys muster sent. Refused, nothing typed.
func TestARetryNeverVouchesForMoreThanMusterTyped(t *testing.T) {
	cs := []Choice{{Picks: []int{0}}, {Picks: []int{1}}, {Picks: []int{0}}}
	p := NewFakePicker(three)
	p.Type("1") // muster's key: question 1 answered
	// Someone answers question 2 exactly as muster would have.
	p.Type("2" + KeyDown + KeyDown + KeyDown + KeyDown + KeyEnter)
	if err := Match(Parse(p.Screen()), Want{Kind: Question, Q: 2, Cursor: 1}, three, nil); err != nil {
		t.Fatalf("setup: %v\n%s", err, p.Screen())
	}
	fp := &fakePane{p: p}
	_, err := DriveFrom(context.Background(), fp, three, cs, &Progress{Choices: cs, Keys: 1}, fast)
	if !errors.Is(err, ErrNotUntouched) || len(fp.sent) != 0 {
		t.Fatalf("a state past muster's keys: %v, sent %q", err, fp.sent)
	}
}

// TestARetryWithOtherAnswersIsRefused: muster typed question 1 as "Red" and
// was lost; a retry with question 1 "Blue" (muster's own card never sends
// one — it resends the recorded answer — but a request could) finds a picker
// that is neither untouched nor this answer's: refused as touched, nothing
// typed. The SAME earlier answer continues.
func TestARetryWithOtherAnswersIsRefused(t *testing.T) {
	old := []Choice{{Picks: []int{0}}, {Picks: []int{1}}, {Picks: []int{0}}}
	p := NewFakePicker(three)
	p.Type("1")
	fp := &fakePane{p: p}
	now := []Choice{{Picks: []int{1}}, {Picks: []int{1}}, {Picks: []int{0}}}
	_, err := DriveFrom(context.Background(), fp, three, now, &Progress{Choices: old, Keys: 1}, fast)
	if !errors.Is(err, ErrNotUntouched) || len(fp.sent) != 0 {
		t.Fatalf("muster's earlier answer in the picker: %v, sent %q", err, fp.sent)
	}
	fp = &fakePane{p: p}
	if _, err := DriveFrom(context.Background(), fp, three, old, &Progress{Choices: old, Keys: 1}, fast); err != nil {
		t.Fatalf("the same answers again: %v (keys %q)", err, fp.sent)
	}
}

// TestAChangedEarlierAnswerIsNotSubmitted: muster typed question 1 and was
// lost; someone then changed question 1's answer in the terminal and came
// back to question 2 — a screen that looks exactly like muster's. The retry
// continues, but the review lists the other answer: refused as someone
// answering in the terminal (not as muster losing track), and the submit is
// never sent.
func TestAChangedEarlierAnswerIsNotSubmitted(t *testing.T) {
	p, keys := lostAfterQ1(t)
	p.mu.Lock()
	p.text[0] = "magenta"
	p.mu.Unlock()
	fp := &fakePane{p: p}
	_, err := DriveFrom(context.Background(), fp, three, threeChoices, &Progress{Choices: threeChoices, Keys: keys}, fast)
	if !errors.Is(err, ErrNotUntouched) || errors.Is(err, ErrLost) || !strings.Contains(err.Error(), "review") {
		t.Fatalf("a changed earlier answer: %v", err)
	}
	if _, _, done := p.Result(); done {
		t.Fatal("a review listing another answer was submitted")
	}
}

// TestALostKeyThatLandedLateIsContinuedPastIt: drive 1's last key DID land,
// after its wait ran out; the retry finds the screen that key made (not the
// one before it) and continues from there — never sending it twice.
func TestALostKeyThatLandedLateIsContinuedPastIt(t *testing.T) {
	p, keys := lostAfterQ1(t)
	p.Type("2") // the lost key, drawn at last: Dog ticked
	fp := &fakePane{p: p}
	if _, err := DriveFrom(context.Background(), fp, three, threeChoices, &Progress{Choices: threeChoices, Keys: keys}, fast); err != nil {
		t.Fatalf("the retry: %v (keys %q)", err, fp.sent)
	}
	if fp.sent[0] == "2" {
		t.Fatalf("the late key was sent again: %q", fp.sent)
	}
	if got, _, _ := p.Result(); !reflect.DeepEqual(got, Answers(three, threeChoices)) {
		t.Fatalf("submitted %v", got)
	}
}

// TestADriveReportsTheKeysItSent: what the next drive's prior is — the keys
// that reached the picker, by how the drive ended.
func TestADriveReportsTheKeysItSent(t *testing.T) {
	// Refused before any key: none.
	touched := NewFakePicker([]askq.Question{pets})
	touched.Type("2")
	if n, err := DriveFrom(context.Background(), &fakePane{p: touched}, []askq.Question{pets}, []Choice{{Picks: []int{0}}}, nil, fast); n != 0 || err == nil {
		t.Fatalf("a touched picker: %d keys, %v", n, err)
	}
	// Lost after its 2nd key: 2.
	p := NewFakePicker([]askq.Question{pets})
	lp := &lookPane{fakePane: &fakePane{p: p}, drop: func(n int) bool { return n == 1 }}
	if n, err := DriveFrom(context.Background(), lp, []askq.Question{pets}, []Choice{{Picks: []int{0, 2}}}, nil, fast); n != 2 || !errors.Is(err, ErrLost) {
		t.Fatalf("lost after key 2: %d keys, %v", n, err)
	}
}

// errPane is fakePane whose Send fails on its n-th key (from 0) — after
// delivering it, as a paste the terminal took but tmux reported failed.
type errPane struct {
	*fakePane
	n int
}

func (e *errPane) Send(ctx context.Context, k string) error {
	at := len(e.sent)
	_ = e.fakePane.Send(ctx, k)
	if at == e.n {
		return errors.New("tmux paste-buffer: lost connection")
	}
	return nil
}

// TestAKeyWhoseSendFailedIsCounted: a key whose send errored may have reached
// the picker, so it counts — the next drive then recognises the screen it made.
func TestAKeyWhoseSendFailedIsCounted(t *testing.T) {
	p := NewFakePicker(three)
	ep := &errPane{fakePane: &fakePane{p: p}, n: 3}
	n, err := DriveFrom(context.Background(), ep, three, threeChoices, nil, fast)
	if err == nil || n != 4 {
		t.Fatalf("a failed send of key 4: %d keys, %v", n, err)
	}
	fp := &fakePane{p: p}
	if _, err := DriveFrom(context.Background(), fp, three, threeChoices, &Progress{Choices: threeChoices, Keys: n}, fast); err != nil {
		t.Fatalf("the retry: %v (keys %q)", err, fp.sent)
	}
}

// TestPriorForOtherQuestionsIsIgnored: a prior that is no answer to these
// questions vouches for nothing.
func TestPriorForOtherQuestionsIsIgnored(t *testing.T) {
	p := NewFakePicker(three)
	p.Type("1")
	fp := &fakePane{p: p}
	_, err := DriveFrom(context.Background(), fp, three, threeChoices, &Progress{Choices: []Choice{{Picks: []int{7}}}, Keys: 1}, fast)
	if !errors.Is(err, ErrNotUntouched) || len(fp.sent) != 0 {
		t.Fatalf("a prior for other questions: %v, sent %q", err, fp.sent)
	}
}

// TestAnAnswerChangedOnTheReviewIsNotSubmitted: the review showed muster's
// answers when its key landed, then someone changed one in the terminal before
// the submit key: the look before that key sees it, and refuses as someone
// answering in the terminal — the submit is never sent.
func TestAnAnswerChangedOnTheReviewIsNotSubmitted(t *testing.T) {
	p := NewFakePicker([]askq.Question{pets})
	seen := 0
	lp := &lookPane{fakePane: &fakePane{p: p}, look: func(int) {
		if Parse(p.Screen()).Kind == Review {
			if seen++; seen == 2 { // the look before the submit key
				p.mu.Lock()
				p.checked[0][2] = true
				p.mu.Unlock()
			}
		}
	}}
	_, err := DriveFrom(context.Background(), lp, []askq.Question{pets}, []Choice{{Picks: []int{0, 2}}}, nil, fast)
	if !errors.Is(err, ErrNotUntouched) || errors.Is(err, ErrLost) || !strings.Contains(err.Error(), "before key") {
		t.Fatalf("an answer changed on the review: %v", err)
	}
	if _, _, done := p.Result(); done {
		t.Fatalf("submitted: %q", lp.sent)
	}
}
