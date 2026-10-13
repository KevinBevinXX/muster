package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestFlowEnqueueSendsTheParamsItWasGiven pins the wire body `flow enqueue`
// builds from its flags: the flow, host, instance, every --param, and the ttl
// in seconds — values chosen pairwise distinct so a swapped field shows.
func TestFlowEnqueueSendsTheParamsItWasGiven(t *testing.T) {
	h := newHarness(t)
	h.json("POST /api/flows/jobs", http.StatusCreated, `{"id":9}`)
	got := h.runCLI("flow", "enqueue", "muse.send", "--host", "lap-x", "--instance", "inst-y",
		"--param", "text=hello = world", "--param", "task=42", "--ttl", "90s")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	reqs := h.nonHealthRequests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v", reqs)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(reqs[0].body), &body); err != nil {
		t.Fatalf("body %q: %v", reqs[0].body, err)
	}
	want := `{"flow":"muse.send","host":"lap-x","instance":"inst-y","params":{"task":"42","text":"hello = world"},"ttlSeconds":90}`
	norm, _ := json.Marshal(body)
	if string(norm) != want {
		t.Fatalf("enqueue body = %s\n want %s", norm, want)
	}
}

func TestFlowEnqueueRefusesAMalformedParamBeforeSending(t *testing.T) {
	for _, args := range [][]string{
		{"flow", "enqueue", "muse.poll", "--param", "novalue"},
		{"flow", "enqueue", "muse.poll", "--param", "=x"},
		{"flow", "enqueue", "muse.poll", "--param", "a=1", "--param", "a=2"},
		{"flow", "enqueue", "muse.poll", "--ttl", "1500ms"},
		{"flow", "status", "0"},
	} {
		h := newHarness(t)
		got := h.runCLI(args...)
		if got.code != exitUsage {
			t.Errorf("%v: exit = %d, want %d (usage); stderr=%q", args, got.code, exitUsage, got.stderr)
		}
		if n := len(h.nonHealthRequests()); n != 0 {
			t.Errorf("%v: %d requests were sent; a usage error must send none", args, n)
		}
	}
}
