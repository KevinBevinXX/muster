package focus

import (
	"errors"
	"strings"
	"testing"
)

// hermesID is a Hermes session id as the pin mints them: "local-<64 hex>",
// 70 characters — six past a Claude Code id's 64-character bound.
var hermesID = "local-" + strings.Repeat("0123456789abcdef", 4)

// TestHermesSubjectIsARuntimeQualifiedMirrorKey: a Hermes conversation's
// subject is the mirror kind keyed "hermes:<id>", valid at 70 characters and
// parsed back from the tools' spelling; a Claude Code id is the key it always
// was. What stays refused: a BARE Hermes id (only the qualified form is
// wider), a bare id past 64, an empty or over-long Hermes id, another prefix.
func TestHermesSubjectIsARuntimeQualifiedMirrorKey(t *testing.T) {
	s := HermesSubject(hermesID)
	if s.Kind != KindMirror || s.Key != "hermes:"+hermesID || s.Valid() != nil {
		t.Fatalf("HermesSubject = %+v (valid: %v)", s, s.Valid())
	}
	got, err := ParseSubject("mirror:hermes:" + hermesID)
	if err != nil || got != s || got.String() != "mirror:hermes:"+hermesID {
		t.Fatalf("ParseSubject(mirror:hermes:<id>) = %+v, %v", got, err)
	}
	if c := MirrorSubject("0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b"); c.Valid() != nil {
		t.Fatalf("a Claude Code subject is no longer valid: %v", c.Valid())
	}
	if c := MirrorSubject(strings.Repeat("c", 64)); c.Valid() != nil {
		t.Fatalf("a 64-character Claude Code id is no longer valid: %v", c.Valid())
	}
	for _, key := range []string{hermesID, strings.Repeat("d", 65), "hermes:", "hermes:" + strings.Repeat("e", 129), "claude:abc", "hermes:-x"} {
		if err := MirrorSubject(key).Valid(); !errors.Is(err, ErrBadSubject) {
			t.Errorf("mirror subject %q: %v, want ErrBadSubject", key, err)
		}
	}
}
