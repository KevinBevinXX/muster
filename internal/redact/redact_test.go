package redact

import (
	"math/rand"
	"strings"
	"testing"
)

// TestEachSecretShapeIsRedacted: each secret shape the
// summary may carry is replaced by «redacted» — the WHOLE output pinned, so a
// redaction that eats the command around it fails too — and look-alikes that
// are not secrets are left as they are. None of the values below is anyone's.
func TestEachSecretShapeIsRedacted(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		// The curl from the audit: a header, userinfo and a token query param.
		{`curl -H "Authorization: Bearer ghp_0123abcd9" https://ci:hunter22@api.example.test/v1/x?page=2&token=abc123`,
			`curl -H "Authorization: «redacted»" https://«redacted»@api.example.test/v1/x?page=2&token=«redacted»`},
		{`curl -H 'Proxy-Authorization: Basic dXNlcjpwYXNz' https://example.test`, `curl -H 'Proxy-Authorization: «redacted»' https://example.test`},
		{`http GET example.test "Authorization:token deadbeef42"`, `http GET example.test "Authorization:«redacted»"`},
		{`grpcurl -H bearer eyJhbGciOi.J9x.y1 example.test:443`, `grpcurl -H bearer «redacted» example.test:443`},
		{`mysql -u app -p s3cret -h db`, `mysql -u app -p «redacted» -h db`},
		{`tool --password=hunter2 --token "abc def" --api-key 'k1'`, `tool --password=«redacted» --token «redacted» --api-key «redacted»`},
		{`curl -u admin:pa55 https://example.test`, `curl -u «redacted» https://example.test`},
		{`AWS_SECRET_ACCESS_KEY=wJalr42 PGPASSWORD='p w' API_TOKEN="t0k" make deploy`, `AWS_SECRET_ACCESS_KEY=«redacted» PGPASSWORD=«redacted» API_TOKEN=«redacted» make deploy`},
		{`export OPENAI_API_KEY=sk-proj-abcdef1234567890`, `export OPENAI_API_KEY=«redacted»`},
		{`echo sk-live-abcdef123456 xoxb-12345678 glpat-abcdefghij12 ghp_0123abcd9`, `echo «redacted» «redacted» «redacted» «redacted»`},
		{`git push https://x:github_pat_11AAAAAAA0abcdefghij@github.example.test/o/r`, `git push https://«redacted»@github.example.test/o/r`},
		{`aws configure set aws_access_key_id AKIAIOSFODNN7EXA`, `aws configure set aws_access_key_id «redacted»`},
		{`muster login mst_abcdef123456 mmt_abcdef123456 mpl_abcdef123456`, `muster login «redacted» «redacted» «redacted»`},
		{`wget "https://cdn.example.test/f?X-Amz-Signature=abcd1234&sig=zz9&api_key=k&v=3"`, `wget "https://cdn.example.test/f?X-Amz-Signature=«redacted»&sig=«redacted»&api_key=«redacted»&v=3"`},
		// Not secrets: left alone.
		{`npm publish --tag next`, `npm publish --tag next`},
		{`git add -u && ps -u root`, `git add -u && ps -u root`},
		{`a basic approach with Bearer tokens`, `a basic approach with Bearer tokens`},
		{`Edit internal/api/x.go`, `Edit internal/api/x.go`},
		{`go test ./internal/mirrorhost -run TestRisky`, `go test ./internal/mirrorhost -run TestRisky`},
	} {
		if got := Secrets(c.in); got != c.want {
			t.Errorf("redact(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

// shapeCorpus is every input TestEachSecretShapeIsRedacted pins, and a few
// spellings only the patterns' case folding matches.
var shapeCorpus = []string{
	`curl -H "Authorization: Bearer ghp_0123abcd9" https://ci:hunter22@api.example.test/v1/x?page=2&token=abc123`,
	`curl -H 'Proxy-Authorization: Basic dXNlcjpwYXNz' https://example.test`,
	`http GET example.test "Authorization:token deadbeef42"`,
	`grpcurl -H bearer eyJhbGciOi.J9x.y1 example.test:443`,
	`mysql -u app -p s3cret -h db`,
	`tool --password=hunter2 --token "abc def" --api-key 'k1'`,
	`curl -u admin:pa55 https://example.test`,
	`AWS_SECRET_ACCESS_KEY=wJalr42 PGPASSWORD='p w' API_TOKEN="t0k" make deploy`,
	`export OPENAI_API_KEY=sk-proj-abcdef1234567890`,
	`echo sk-live-abcdef123456 xoxb-12345678 glpat-abcdefghij12 ghp_0123abcd9`,
	`git push https://x:github_pat_11AAAAAAA0abcdefghij@github.example.test/o/r`,
	`aws configure set aws_access_key_id AKIAIOSFODNN7EXA`,
	`muster login mst_abcdef123456 mmt_abcdef123456 mpl_abcdef123456`,
	`wget "https://cdn.example.test/f?X-Amz-Signature=abcd1234&sig=zz9&api_key=k&v=3"`,
	`npm publish --tag next`, `git add -u && ps -u root`, `a basic approach with Bearer tokens`,
	// Case folding: upper case, the Kelvin sign (folds with k) and the long
	// s (folds with s) — each matched by a (?i) pattern.
	`AUTHORIZATION: BEARER ABC123DEF`, `MYSQL -P S3CRET`, "API_\u212aEY=v4lue", `PAſſWORD=hunter2`, `https://x.example.test/?ſig=abc9`,
	"api_\u212aey=v4lue", "BAſIC dXNlcjpwYXNz42",
	// One input per literal the filter looks for, so dropping any one of
	// them from the filter is seen.
	`X_SECRET=s1`, `X_PASSWD=s2`, `X_PWD=s3`,
	`https://h.example.test/?tokenx=a1`, `https://h.example.test/?keyx=a2`, `https://h.example.test/?secretx=a3`,
	`https://h.example.test/?passx=a4`, `https://h.example.test/?authx=a5`, `https://h.example.test/?sigx=a6`,
	`gho_abcdef123`, `ghu_abcdef123`, `ghs_abcdef123`, `ghr_abcdef123`, `github_pat_abcdef123`,
	`xoxa-12345678`, `xoxp-12345678`, `xoxo-12345678`, `xoxs-12345678`, `xoxr-12345678`,
	`Basic dXNlcjpwYXNz42`,
}

// TestTheFilterNeverChangesWhatIsRedacted: Secrets (each pattern skipped when
// the literal it needs is absent) and secrets with every pattern run agree on
// every shape, its case-folded spellings, and many random mixtures of them
// with prose — so the filter is a cost saving, never a change of what is
// redacted.
func TestTheFilterNeverChangesWhatIsRedacted(t *testing.T) {
	corpus := append([]string(nil), shapeCorpus...)
	frag := append(append([]string(nil), corpus...), "the quick brown fox", "x=1", "a@b", "://", "--", "-p", "key", "Token", "=", " ", "\n")
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for j := r.Intn(6); j >= 0; j-- {
			f := frag[r.Intn(len(frag))]
			if r.Intn(4) == 0 && len(f) > 2 { // a slice of a shape, cut anywhere
				a := r.Intn(len(f))
				f = f[a : a+r.Intn(len(f)-a)+1]
			}
			b.WriteString(f)
		}
		corpus = append(corpus, b.String())
	}
	differ := 0
	for _, c := range corpus {
		if got, want := Secrets(c), secrets(c, false); got != want {
			t.Errorf("filtered %q\n got %q\nwant %q", c, got, want)
		}
		if secrets(c, false) != c {
			differ++
		}
	}
	// CONTROL: the corpus exercises redaction, not just prose.
	if differ < 1000 {
		t.Fatalf("only %d of %d inputs had anything redacted", differ, len(corpus))
	}
}
