package agents

import "testing"

// TestAJobWorktreeIsASiblingOfTheWorkingDirectory pins the layout both sides
// of the wire derive, and what IsJobWorktreeDir admits.
func TestAJobWorktreeIsASiblingOfTheWorkingDirectory(t *testing.T) {
	if got := JobWorktreeDir("/data/workspace/widgets", 17); got != "/data/workspace/.muster-jobs/17" {
		t.Fatalf("JobWorktreeDir = %q", got)
	}
	if got := JobWorktreesDir("/data/workspace/widgets/"); got != "/data/workspace/.muster-jobs" {
		t.Fatalf("JobWorktreesDir = %q", got)
	}
	for dir, want := range map[string]bool{
		"/data/workspace/.muster-jobs/17":                   true,
		"/data/workspace/.muster-jobs/9223372036854775807":  true,
		"/data/workspace/.muster-jobs/0":                    false,
		"/data/workspace/.muster-jobs/017":                  false,
		"/data/workspace/.muster-jobs/-1":                   false,
		"/data/workspace/.muster-jobs/x":                    false,
		"/data/workspace/.muster-jobs/17/":                  false,
		"/data/workspace/.muster-jobs/17/sub":               false,
		"/data/workspace/.muster-jobs/../.muster-jobs/17":   false,
		"/data/workspace/widgets/.muster-jobs/17":           false,
		"/data/workspace/widgets":                           false,
		"/data/.muster-jobs/17":                             false,
		"data/workspace/.muster-jobs/17":                    false,
		"":                                                  false,
		"/data/workspace/.muster-jobs/12345678901234567890": false,
	} {
		if got := IsJobWorktreeDir("/data/workspace/widgets", dir); got != want {
			t.Errorf("IsJobWorktreeDir(%q) = %v, want %v", dir, got, want)
		}
	}
}
