package hermeshost

import (
	"slices"
	"strings"
	"unicode"
)

// ViLabels is the vi-mode label of Hermes's status bar in every language
// Hermes bundles at the pin, by language: cli.status_bar.vim_insert,
// vim_replace and vim_normal of P:locales/<lang>.yaml, for the 17 of
// SUPPORTED_LANGUAGES (P:agent/i18n.py:33-36).
//
// 🔴 EXTRACTED FROM THE PINNED SOURCE, AND CHECKED AGAINST IT:
// TestViLabelsMatchThePinnedLocales (build tag livehermes, `make
// test-livehermes`) re-reads the installed Hermes's locales and fails on any
// difference, so a pin bump re-extracts them.
//
// ⚠ DEFENCE IN DEPTH, NEVER A GATE (docs/design/hermes.md, vi mode): the label
// is the last status-bar fragment, the first a narrow pane trims, absent when
// the bar is hidden, and a plugin pack or a user overlay can change it. A host
// whose Hermes uses vi mode must not enable -hermes-send at all.
var ViLabels = map[string][3]string{
	"af":      {"INVOEG", "VERVANG", "NORMAAL"},
	"ar":      {"إدراج", "استبدال", "عادي"},
	"de":      {"EINFÜGEN", "ERSETZEN", "NORMAL"},
	"en":      {"INSERT", "REPLACE", "NORMAL"},
	"es":      {"INSERTAR", "REEMPLAZAR", "NORMAL"},
	"fr":      {"INSERTION", "REMPLACEMENT", "NORMAL"},
	"ga":      {"IONSÁIGH", "IONADAIGH", "GNÁTH"},
	"hu":      {"BESZÚRÁS", "CSERE", "NORMÁL"},
	"it":      {"INSERT", "REPLACE", "NORMAL"},
	"ja":      {"INSERT", "REPLACE", "NORMAL"},
	"ko":      {"INSERT", "REPLACE", "NORMAL"},
	"pt":      {"INSERIR", "SUBSTITUIR", "NORMAL"},
	"ru":      {"ВСТАВКА", "ЗАМЕНА", "ОБЫЧНЫЙ"},
	"tr":      {"EKLE", "DEĞİŞTİR", "NORMAL"},
	"uk":      {"ВСТАВКА", "ЗАМІНА", "ЗВИЧАЙНИЙ"},
	"zh":      {"插入", "替换", "普通"},
	"zh-hant": {"INSERT", "REPLACE", "NORMAL"},
}

// viWords is every label, once.
var viWords = func() []string {
	var out []string
	for _, ls := range ViLabels {
		for _, l := range ls {
			if !slices.Contains(out, l) {
				out = append(out, l)
			}
		}
	}
	return out
}()

// HasViLabel reports whether status-bar text holds a vi-mode label as a
// whole word, in any bundled language. A session title that happens to hold
// one is a false refusal, which is safe.
func HasViLabel(text string) bool {
	for _, w := range viWords {
		for i := 0; ; {
			j := strings.Index(text[i:], w)
			if j < 0 {
				break
			}
			at := i + j
			if boundary(text, at, -1) && boundary(text, at+len(w), 1) {
				return true
			}
			i = at + len(w)
		}
	}
	return false
}

// boundary reports whether the rune before (dir -1) or at (dir 1) byte
// offset at in s is the edge of a word: none, a space or punctuation.
func boundary(s string, at, dir int) bool {
	var r rune
	if dir < 0 {
		if at == 0 {
			return true
		}
		r = []rune(s[:at])[len([]rune(s[:at]))-1]
	} else {
		if at >= len(s) {
			return true
		}
		r = []rune(s[at:])[0]
	}
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}
