package hermeshost

import (
	"errors"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// THE HOOKS HERMES DECLARES (round-1 audit, #191 🟡1): the consent check has to
// compare the allowlist with the commands Hermes's config ACTUALLY runs, not
// with this binary's path — on Nix the two differ whenever the hook was
// declared with another store path, and a check of the wrong one both lies
// ("ok") and suggests a writer run that would remove the declared command's
// consent.
//
// 🔴 ONLY THE hooks: SECTION, AND ONLY FOR THE DIAGNOSTIC. config.yaml can
// carry inline keys, so it is read by `-list -hermes` alone (DiagnosticReadPaths;
// the daemon's read path never opens it, which the ledger test holds), decoded
// into a struct that has no other field — nothing else in the file is kept —
// and no error ever quotes it.

// DeclaredHook is one `hooks:` entry: an event and its command.
type DeclaredHook struct {
	Event   string
	Command string
}

// DiagnosticReadPaths is what `-list -hermes` may open under a Hermes home:
// the daemon's ReadPaths and config.yaml, of which it decodes `hooks:` only.
var DiagnosticReadPaths = append(append([]string(nil), ReadPaths...), "config.yaml")

// maxConfig bounds a config.yaml read.
const maxConfig = 1 << 20

// ErrConfigUnreadable: config.yaml is not YAML whose hooks section is the
// shape Hermes reads. Never the decoder's error, which quotes the file.
var ErrConfigUnreadable = errors.New("config.yaml's hooks section could not be read")

// DeclaredHooks is every `hooks:` entry of h's config.yaml, in file order
// (event by event); none, and no error, when there is no config.yaml.
func (h Home) DeclaredHooks() ([]DeclaredHook, error) {
	b, err := h.readConfig()
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, nil
	}
	var doc struct {
		Hooks yaml.Node `yaml:"hooks"`
	}
	if yaml.Unmarshal(b, &doc) != nil {
		return nil, ErrConfigUnreadable
	}
	if doc.Hooks.Kind == 0 {
		return nil, nil
	}
	if doc.Hooks.Kind != yaml.MappingNode {
		return nil, ErrConfigUnreadable
	}
	var out []DeclaredHook
	for i := 0; i+1 < len(doc.Hooks.Content); i += 2 {
		event, list := doc.Hooks.Content[i].Value, doc.Hooks.Content[i+1]
		if list.Kind != yaml.SequenceNode {
			continue // Hermes warns and skips it too (_parse_hooks)
		}
		for _, item := range list.Content {
			var e struct {
				Command string `yaml:"command"`
			}
			if item.Kind != yaml.MappingNode || item.Decode(&e) != nil {
				continue
			}
			if c := strings.TrimSpace(e.Command); c != "" {
				out = append(out, DeclaredHook{Event: event, Command: c})
			}
		}
	}
	return out, nil
}

// MusterHookCommand reports whether command is a muster-mirror hermes-hook
// command (any binary path).
func MusterHookCommand(command string) bool { return musterHookCommand(command) }
