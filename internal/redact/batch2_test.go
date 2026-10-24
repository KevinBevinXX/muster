package redact

import (
	"strings"
	"testing"
)

// THE SECOND BATCH: the 13 shapes an audit found passing the first batch.
// Every value below is synthetic and spelled obviously fake (FAKE…, 0000…,
// "example"). A few are assembled at run time ("AGE-SECRET-" + "KEY-…",
// "-----BEGIN " + "…PRIVATE KEY-----") so no single source line has the
// shape tests/leakscan.py refuses: the gate scans lines, and a fake that LOOKS
// real on one line is exactly what it exists to stop.

var (
	pemBegin = "-----BEGIN " + "OPENSSH PRIVATE KEY-----"
	pemEnd   = "-----END " + "OPENSSH PRIVATE KEY-----"
	ageKey   = "AGE-SECRET-" + "KEY-1FAKEFAKEFAKEFAKEFAKE0000000000"
	awsFake  = "FAKEawsSECRETkey0000000000example0000000" // 40 characters
)

func batch2Cases() []struct{ name, in, want string } {
	return []struct{ name, in, want string }{
		{"stripe sk_live", "stripe key sk_live_FAKE0000example", "stripe key «redacted»"},
		{"stripe sk_test", "use sk_test_FAKE0000example here", "use «redacted» here"},
		{"stripe rk_live", "restricted rk_live_FAKE0000example", "restricted «redacted»"},
		{"aws secret access key", "aws secret access key " + awsFake + " in the profile", "aws secret access key «redacted» in the profile"},
		{"jwt", "token eyJFAKEhdr0.eyJFAKEbody0.FAKEsignature0 sent", "token «redacted» sent"},
		{"json api_key", `{"api_key": "FAKE-0000-example", "region": "x"}`, `{"api_key": "«redacted»", "region": "x"}`},
		{"json token", `{"token":"FAKE0000"}`, `{"token":"«redacted»"}`},
		{"json secret", `{"client_secret" : "FAKE0000"}`, `{"client_secret" : "«redacted»"}`},
		{"json password", `{"password": "FAKEpass0000"}`, `{"password": "«redacted»"}`},
		{"yaml password", "db:\n  password: FAKEpass0000\n  host: db.example.test", "db:\n  password: «redacted»\n  host: db.example.test"},
		{"yaml token", "token: FAKE0000example", "token: «redacted»"},
		{"spaced assignment", "API_KEY = 'FAKE0000example'", "API_KEY = «redacted»"},
		{"said password", "the password is FAKEpass0000, use it", "the password is «redacted», use it"},
		{"age secret key", "key " + ageKey, "key «redacted»"},
		{"openssh private key block", "here:\n" + pemBegin + "\nFAKEFAKEFAKE0000\nFAKEFAKEFAKE0000\n" + pemEnd + "\nthen", "here:\n«redacted»\nthen"},
		{"google api key", "maps key AIzaFAKE0000example0000FAKE0000ab", "maps key «redacted»"},
		{"discord bot token", "bot MFAKEFAKEFAKEFAKEFAKE0000.FAKE00.FAKEFAKEFAKEFAKEFAKEFAKE00000 ok", "bot «redacted» ok"},
		{"hugging face token", "HF hf_FAKEFAKEFAKE0000example0000", "HF «redacted»"},
		{"x-api-key header", `curl -H "X-Api-Key: FAKE0000example" https://api.example.test`, `curl -H "X-Api-Key: «redacted»" https://api.example.test`},
		{"api-key header", "Api-Key=FAKE0000example", "Api-Key=«redacted»"},
		{"authorization header", "Authorization: FAKE0000example", "Authorization: «redacted»"},
		// Not secrets: left alone.
		{"git sha near aws", "aws deploy at 0123456789abcdef0123456789abcdef01234567", "aws deploy at 0123456789abcdef0123456789abcdef01234567"},
		{"prose about a key", "the key point is speed", "the key point is speed"},
		{"a dotted version", "Moved to v1.2.3 and README.md", "Moved to v1.2.3 and README.md"},
	}
}

// TestEachSecondBatchShapeIsRedacted: each shape is replaced — the WHOLE
// output pinned, so a redaction that eats its surroundings fails too — and
// the look-alikes are left alone.
func TestEachSecondBatchShapeIsRedacted(t *testing.T) {
	for _, c := range batch2Cases() {
		if got := Secrets(c.in); got != c.want {
			t.Errorf("%s: redact(%q)\n got %q\nwant %q", c.name, c.in, got, c.want)
		}
	}
}

// The filter-equivalence test (TestTheFilterNeverChangesWhatIsRedacted) runs
// over these too.
func init() {
	for _, c := range batch2Cases() {
		shapeCorpus = append(shapeCorpus, c.in)
	}
	shapeCorpus = append(shapeCorpus, strings.ToUpper("the password is fakepass0000"), "PASſWORD: FAKE0000")
}
