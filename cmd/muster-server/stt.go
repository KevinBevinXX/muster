package main

import (
	"fmt"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/stt"
)

// validateSTT is the deployment policy for dictation (internal/stt).
//
// 🔴 THE TOKEN ALONE TURNS IT ON, AND A URL WITHOUT A TOKEN IS A VALID, INERT
// STATE — the memo relay's rule, for the memo relay's reason: the manifest can
// name the service before the secret holding its credential exists, and
// refusing that would make the rollout order impossible. A token with no URL is
// refused: a credential with nowhere to go would leave the microphone off with
// nothing in the log but a banner line.
//
// The URL's shape is checked whenever it is set, token or not, so a typo is
// found by the boot that names it rather than by the one that enables it.
func (c config) validateSTT() error {
	if c.STTURL != "" {
		if _, err := stt.New(c.STTURL, "shape-check", 0); err != nil {
			return fmt.Errorf("invalid %s: %w", envSTTURL, err)
		}
	}
	if c.STTToken != "" && c.STTURL == "" {
		return fmt.Errorf("%s is set but %s is not: dictation has a credential and no service to present it "+
			"to. Set %s to the speech-to-text service's base URL (http(s)://host[:port]), or unset %s",
			envSTTToken, envSTTURL, envSTTURL, envSTTToken)
	}
	return nil
}

// buildTranscriber is the message box's speech-to-text client, or nil (no
// microphone anywhere) when dictation is off.
//
// 🔴 IT RETURNS THE INTERFACE ONLY WHEN THERE IS A CLIENT. A nil *stt.Client
// stored in api.Transcriber is a NON-nil interface, which would draw a
// microphone on every page whose every recording then panics.
func buildTranscriber(cfg config) (api.Transcriber, error) {
	if cfg.STTToken == "" {
		return nil, nil
	}
	c, err := stt.New(cfg.STTURL, cfg.STTToken, 0)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// logSTTBanner states dictation's posture in both directions. It names the
// service's URL and never the token.
func (a *app) logSTTBanner(ext api.Extensions) {
	if ext.Transcriber == nil {
		a.logger.Printf("dictation: OFF — %s is unset (%s: %s), so no message box draws a microphone and "+
			"POST /transcribe answers 404", envSTTToken, envSTTURL, presence(a.cfg.STTURL))
		return
	}
	a.logger.Printf("dictation: ON — %s is set; every message box draws a microphone, and recordings are "+
		"transcribed by %s=%s (the token stays in this process; the browser never sees it)",
		envSTTToken, envSTTURL, a.cfg.STTURL)
}
