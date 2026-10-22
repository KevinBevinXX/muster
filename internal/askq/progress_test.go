package askq

import "testing"

// TestWhatADriveLeftIsHandedToTheNext: a settled failure records what the
// drive left in the picker (Left) and whether it can be tried again (Retry);
// the next BeginTyping returns it — what that drive is handed — and its own
// outcome replaces it; a success clears it. An unsettled failure records
// nothing it may have typed.
func TestWhatADriveLeftIsHandedToTheNext(t *testing.T) {
	s, c := testStore(t)
	if _, err := s.Open(key(1), twoQs, ""); err != nil {
		t.Fatal(err)
	}
	left := &Progress{Choices: []Choice{{Picks: []int{1}}, {Picks: []int{0}}}, Keys: 2}
	if _, _, err := s.BeginTyping(key(1), someAnswers); err != nil {
		t.Fatal(err)
	}
	e, _ := s.TypingFailed(key(1), Failure{Note: "lost", Settled: true, Left: left, Retry: true})
	if e.State != Failed || !e.Retry || e.Left == nil || e.Left.Keys != 2 {
		t.Fatalf("a settled failure: %+v", e)
	}
	// The record is a copy: the caller's later changes do not reach it.
	left.Choices[0].Picks[0] = 9
	got, _, err := s.BeginTyping(key(1), someAnswers)
	if err != nil || got.Left == nil || got.Left.Keys != 2 || got.Left.Choices[0].Picks[0] != 1 || got.Retry {
		t.Fatalf("the next drive is handed %+v (%v)", got.Left, err)
	}
	// This drive's outcome replaces it.
	e, _ = s.TypingFailed(key(1), Failure{Note: "no picker", Settled: true})
	if e.Left != nil || e.Retry {
		t.Fatalf("a drive that left nothing: %+v", e)
	}
	// An unsettled one: nothing recorded.
	c.add(MaxDrive)
	_, _, _ = s.BeginTyping(key(1), someAnswers)
	e, _ = s.TypingFailed(key(1), Failure{Note: "timeout", Settled: false, Left: left})
	if e.Left != nil {
		t.Fatalf("an unsettled failure: %+v", e)
	}
	// A success clears both.
	c.add(MaxDrive)
	_, _, _ = s.BeginTyping(key(1), someAnswers)
	s.TypingDone(key(1))
	if e, _ := s.Find(key(1)); e.Left != nil {
		t.Fatalf("after a success: %+v", e)
	}
}

// TestAPreviewQuestionTakesNoAnswerOfOnesOwn: the terminal's preview picker
// has no row to type one into (Question.Previews), so the form refuses it;
// a multi-select question's previews are not drawn, and it still takes one.
func TestAPreviewQuestionTakesNoAnswerOfOnesOwn(t *testing.T) {
	pv := Question{Question: "Which color?", Options: []Option{{Label: "Red", Preview: "warm"}, {Label: "Blue"}}}
	if !pv.Previews() {
		t.Fatal("a single-select question with a preview is drawn with previews")
	}
	if _, _, err := FromForm([]Question{pv}, map[string][]string{"a0": {OtherValue}, "o0": {"teal"}}); err == nil {
		t.Fatal("an answer of one's own on a preview question was accepted")
	}
	if _, _, err := FromForm([]Question{pv}, map[string][]string{"a0": {"Blue"}}); err != nil {
		t.Fatalf("an option of a preview question: %v", err)
	}
	ms := pv
	ms.MultiSelect = true
	if ms.Previews() {
		t.Fatal("a multi-select question is drawn without previews")
	}
	if _, _, err := FromForm([]Question{ms}, map[string][]string{"a0": {OtherValue}, "o0": {"teal"}}); err != nil {
		t.Fatalf("an answer of one's own on a multi-select question: %v", err)
	}
	// The call's preview is read, and bounded.
	qs, err := ParseQuestions([]byte(`[{"question":"Q?","header":"H","options":[{"label":"A","preview":"line 1\nline 2"},{"label":"B"}]}]`))
	if err != nil || qs[0].Options[0].Preview != "line 1\nline 2" {
		t.Fatalf("a preview: %+v %v", qs, err)
	}
	long := make([]byte, MaxPreview+1)
	for i := range long {
		long[i] = 'x'
	}
	if _, err := ParseQuestions([]byte(`[{"question":"Q?","options":[{"label":"A","preview":"` + string(long) + `"}]}]`)); err == nil {
		t.Fatal("a preview past MaxPreview was accepted")
	}
}
