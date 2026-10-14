package main

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/api"
)

// TestDictationIsReadFromItsTwoVariables: the names in the manifest are the
// names read, trimmed.
func TestDictationIsReadFromItsTwoVariables(t *testing.T) {
	env := map[string]string{
		"MUSTER_STT_URL":   " http://stt.example.invalid:8000/ ",
		"MUSTER_STT_TOKEN": " stt-credential-fixture\n",
	}
	c, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.STTURL != "http://stt.example.invalid:8000" || c.STTToken != "stt-credential-fixture" {
		t.Fatalf("STTURL=%q STTToken=%q; want the two values, trimmed", c.STTURL, c.STTToken)
	}
}

// TestDictationValidation: the token turns it on; a URL alone is inert and
// valid; a token with no URL, or a URL that is not http(s)://host[:port], is
// refused at boot.
func TestDictationValidation(t *testing.T) {
	if err := memoBase().validate(); err != nil {
		t.Fatalf("control: the base configuration is refused: %v", err)
	}
	ok := func(url, token string) {
		t.Helper()
		c := memoBase()
		c.STTURL, c.STTToken = url, token
		if err := c.validate(); err != nil {
			t.Errorf("url=%q token set=%v refused: %v", url, token != "", err)
		}
	}
	bad := func(url, token, mention string) {
		t.Helper()
		c := memoBase()
		c.STTURL, c.STTToken = url, token
		err := c.validate()
		if err == nil || !strings.Contains(err.Error(), mention) {
			t.Errorf("url=%q token set=%v: err=%v; want a refusal naming %s", url, token != "", err, mention)
		}
		if err != nil && token != "" && strings.Contains(err.Error(), token) {
			t.Errorf("the refusal prints the token: %v", err)
		}
	}
	ok("", "")
	ok("http://stt.example.invalid:8000", "")
	ok("http://stt.example.invalid:8000", "stt-credential-fixture")
	bad("", "stt-credential-fixture", envSTTURL)
	bad("stt.example.invalid:8000", "", envSTTURL)
	bad("http://stt.example.invalid:8000/v1/audio/transcriptions", "stt-credential-fixture", envSTTURL)
}

// TestTheTranscriberExistsOnlyWithAToken: no token, no client — and no typed
// nil in the interface (which would draw a microphone over a nil client).
func TestTheTranscriberExistsOnlyWithAToken(t *testing.T) {
	c := memoBase()
	c.STTURL = "http://stt.example.invalid:8000"
	tr, err := buildTranscriber(c)
	if err != nil || tr != nil {
		t.Fatalf("with no token: %v, %v; want a nil interface", tr, err)
	}
	c.STTToken = "stt-credential-fixture"
	if tr, err = buildTranscriber(c); err != nil || tr == nil {
		t.Fatalf("with a token: %v, %v; want a client", tr, err)
	}
}

// TestTheDictationBannerNamesTheURLAndNeverTheToken.
func TestTheDictationBannerNamesTheURLAndNeverTheToken(t *testing.T) {
	c := memoBase()
	c.STTURL, c.STTToken = "http://stt.example.invalid:8000", "stt-credential-fixture"
	tr, _ := buildTranscriber(c)
	var on, off bytes.Buffer
	(&app{cfg: c, logger: log.New(&on, "", 0)}).logSTTBanner(api.Extensions{Transcriber: tr})
	(&app{cfg: memoBase(), logger: log.New(&off, "", 0)}).logSTTBanner(api.Extensions{})
	if !strings.Contains(on.String(), "dictation: ON") || !strings.Contains(on.String(), c.STTURL) {
		t.Errorf("ON line = %q", on.String())
	}
	if strings.Contains(on.String(), c.STTToken) {
		t.Errorf("the banner prints the token: %q", on.String())
	}
	if !strings.Contains(off.String(), "dictation: OFF") {
		t.Errorf("OFF line = %q", off.String())
	}
}
