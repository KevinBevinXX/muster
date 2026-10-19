package cctranscript

import (
	"encoding/json"
	"reflect"
	"testing"
)

// userLine is a main-conversation user record with a string content, the shape
// the CLI writes for a prompt AND for every harness notice. origin "" = none.
func userLine(t *testing.T, content, origin string) Record {
	t.Helper()
	m := map[string]any{"type": "user", "promptId": "p", "message": map[string]any{"role": "user", "content": content}}
	if origin != "" {
		m["origin"] = map[string]any{"kind": origin}
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return rec(t, string(b))
}

// The shapes are the real CLI's (2.1.2xx, see notice.go) with invented content.
func TestEventsClassifiesHarnessNotices(t *testing.T) {
	const task = "<task-notification>\n<task-id>a1b2c3</task-id>\n<tool-use-id>toolu_x</tool-use-id>\n" +
		"<output-file>/tmp/example/tasks/a1b2c3.output</output-file>\n<status>completed</status>\n" +
		"<summary>Agent \"Audit the widget\" finished</summary>\n<note>A note the harness adds.</note>\n" +
		"<result>The widget is fine.\n\nTwo nits.</result>\n<usage><total_tokens>9</total_tokens></usage>\n</task-notification>"
	const monitor = "<task-notification>\n<task-id>m9</task-id>\n<summary>Monitor event: \"CI on the example PR\"</summary>\n" +
		"<event>ci/unit: success\nci/lint: failure</event>\n</task-notification>"
	cases := []struct {
		name, content, origin string
		want                  Event
	}{
		{"a finished agent: its status, summary and result", task, "task-notification",
			Event{Kind: KindNotice, Notice: NoticeTask, Status: "completed", Text: `Agent "Audit the widget" finished`, Output: "The widget is fine.\n\nTwo nits.", TaskToolID: "toolu_x"}},
		{"the same with no origin (CLI before 2.1.22): the tag alone decides", task, "",
			Event{Kind: KindNotice, Notice: NoticeTask, Status: "completed", Text: `Agent "Audit the widget" finished`, Output: "The widget is fine.\n\nTwo nits.", TaskToolID: "toolu_x"}},
		{"a monitor event: no status, the event is the detail", monitor, "task-notification",
			Event{Kind: KindNotice, Notice: NoticeTask, Text: `Monitor event: "CI on the example PR"`, Output: "ci/unit: success\nci/lint: failure"}},
		{"a tagless task notification is told by its origin", "Background agent \"Audit the widget\" was stopped by the user.", "task-notification",
			Event{Kind: KindNotice, Notice: NoticeTask, Text: "Background agent \"Audit the widget\" was stopped by the user.", Output: "Background agent \"Audit the widget\" was stopped by the user."}},
		// 🔴 A SLASH COMMAND IS THE HUMAN'S: a prompt bubble showing what they
		// typed, never a notice row and never the raw tags.
		{"a slash command (name first) is the human's prompt", "<command-name>/model</command-name>\n            <command-message>model</command-message>\n            <command-args>sonnet</command-args>", "",
			Event{Kind: KindPrompt, Text: "/model sonnet"}},
		{"a slash command (message first, human origin) is the human's prompt", "<command-message>handoff</command-message>\n<command-name>/handoff</command-name>", "human",
			Event{Kind: KindPrompt, Text: "/handoff"}},
		{"a prompt that pastes command tags and writes under them keeps its text", "<command-name>/model</command-name>\nwhy?", "human",
			Event{Kind: KindPrompt, Text: "<command-name>/model</command-name>\nwhy?"}},
		{"a local command's output, escapes stripped", "<local-command-stdout>Set model to \x1b[1mExample\x1b[22m\nsaved</local-command-stdout>", "",
			Event{Kind: KindNotice, Notice: NoticeCommandOutput, Text: "Set model to Example", Output: "Set model to Example\nsaved"}},

		// 🔴 A HUMAN WHO MENTIONS THE TAG IS STILL THE HUMAN.
		{"a prompt that mentions the tag mid-text", "why did the <task-notification> say failed?", "human",
			Event{Kind: KindPrompt, Text: "why did the <task-notification> say failed?"}},
		{"a prompt that pastes a notification and writes under it", task + "\n\nwhat does this mean?", "human",
			Event{Kind: KindPrompt, Text: task + "\n\nwhat does this mean?"}},
		{"a prompt that is wholly an unknown tag", "<thoughts>keep it short</thoughts>", "human",
			Event{Kind: KindPrompt, Text: "<thoughts>keep it short</thoughts>"}},
		{"an unclosed notice tag is not a notice", "<task-notification>\n<summary>half", "",
			Event{Kind: KindPrompt, Text: "<task-notification>\n<summary>half"}},

		// 🔴 A HUMAN-ORIGIN RECORD IS NEVER A NOTICE BY ITS TAGS, even when it is
		// wholly a pasted notice block: the CLI says the human wrote it.
		{"a human-origin prompt that is wholly a task notification", task, "human",
			Event{Kind: KindPrompt, Text: task}},
		{"a human-origin prompt that is wholly local command output", "<local-command-stdout>Set model to Example</local-command-stdout>", "human",
			Event{Kind: KindPrompt, Text: "<local-command-stdout>Set model to Example</local-command-stdout>"}},

		// 🔴 THE SLASH-COMMAND REWRITE IS ONLY FOR A PURE COMMAND RECORD: any
		// other block would be dropped by the rewrite, so the raw text is shown.
		{"a command record with any other block is shown raw", "<command-name>/model</command-name>\n<note>secret extra instruction</note>", "human",
			Event{Kind: KindPrompt, Text: "<command-name>/model</command-name>\n<note>secret extra instruction</note>"}},
		{"the same with no origin is shown raw too", "<command-message>model</command-message>\n<command-name>/model</command-name>\n<note>extra</note>", "",
			Event{Kind: KindPrompt, Text: "<command-message>model</command-message>\n<command-name>/model</command-name>\n<note>extra</note>"}},
		{"args before the name is still the command", "<command-args>sonnet</command-args>\n<command-name>/model</command-name>", "human",
			Event{Kind: KindPrompt, Text: "/model sonnet"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Events(userLine(t, c.content, c.origin))
			if want := []Event{c.want}; !reflect.DeepEqual(got, want) {
				t.Fatalf("events:\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

// A notice in array content (text blocks, the way a prompt with an image is
// written) is classified the same: PromptText joins the blocks first.
func TestEventsNoticeInBlockContent(t *testing.T) {
	r := rec(t, `{"type":"user","message":{"content":[{"type":"text","text":"<task-notification><status>failed</status><summary>Agent \"x\" failed</summary></task-notification>"}]}}`)
	got := Events(r)
	want := []Event{{Kind: KindNotice, Notice: NoticeTask, Status: "failed", Text: `Agent "x" failed`}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events:\n got %+v\nwant %+v", got, want)
	}
}

// 🔴 A task notice's status, summary and detail are the notice's OWN direct
// children, never a tag quoted inside its result or event (a background task's
// output can print anything, including the notice's own tags).
func TestEventsNoticeIgnoresTagsNestedInTaskOutput(t *testing.T) {
	cases := []struct {
		name, content string
		want          Event
	}{
		{"a spoofed status and summary inside the result",
			"<task-notification>\n<task-id>t1</task-id>\n<result>step one\n<status>completed</status>\n<summary>All good</summary></result>\n<status>failed</status>\n</task-notification>",
			Event{Kind: KindNotice, Notice: NoticeTask, Status: "failed", Text: "step one",
				Output: "step one\n<status>completed</status>\n<summary>All good</summary>"}},
		{"a spoofed result inside a monitor event",
			"<task-notification>\n<task-id>m1</task-id>\n<event>ci: red <result>ci: green</result></event>\n<summary>Monitor event</summary>\n</task-notification>",
			Event{Kind: KindNotice, Notice: NoticeTask, Text: "Monitor event", Output: "ci: red <result>ci: green</result>"}},
		{"text between the children is skipped, not an error",
			"<task-notification>\nloose words <status>killed</status> more <summary>Stopped</summary></task-notification>",
			Event{Kind: KindNotice, Notice: NoticeTask, Status: "killed", Text: "Stopped"}},
		// An unclosed top-level tag has no body to jump over: innerTag looks
		// past its opening, so the children after it are still read.
		{"an unclosed child does not hide the children after it",
			"<task-notification>\n<task-id>u1</task-id>\n<br>\n<status>failed</status>\n<summary>Agent \"y\" failed</summary>\n<result>it broke</result>\n</task-notification>",
			Event{Kind: KindNotice, Notice: NoticeTask, Status: "failed", Text: `Agent "y" failed`, Output: "it broke"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Events(userLine(t, c.content, "task-notification"))
			if want := []Event{c.want}; !reflect.DeepEqual(got, want) {
				t.Fatalf("events:\n got %+v\nwant %+v", got, want)
			}
		})
	}
}
