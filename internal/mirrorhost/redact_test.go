package mirrorhost

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// TestASecretInAPendingActionIsRedactedOnTheHost: each secret shape the
// summary may carry is replaced by «redacted» — the WHOLE output pinned, so a
// redaction that eats the command around it fails too — and look-alikes that
// are not secrets are left as they are. None of the values below is anyone's.
func TestASecretInAPendingActionIsRedactedOnTheHost(t *testing.T) {
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
		if got := redactSecrets(c.in); got != c.want {
			t.Errorf("redact(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

// TestThePendingSummaryAndLastWordsAreRedacted: the redaction is on the path
// that leaves the host — PendingOf's summary and last text — not only in the
// helper.
func TestThePendingSummaryAndLastWordsAreRedacted(t *testing.T) {
	p := PendingOf([]cctranscript.Event{
		{Kind: cctranscript.KindText, Text: "Using GITHUB_TOKEN=ghp_0123456789abcdef to push."},
		call("t1", "Bash", map[string]string{"command": `curl -H "Authorization: Bearer abc123def" https://example.test`}),
	})
	if p == nil || p.Summary != `Bash: curl -H "Authorization: «redacted»" https://example.test` || p.Tool != "Bash" ||
		p.Last != "Using GITHUB_TOKEN=«redacted» to push." {
		t.Fatalf("pending %+v", p)
	}
	for _, f := range []string{p.Summary, p.Last} {
		if strings.Contains(f, "abc123def") || strings.Contains(f, "ghp_") {
			t.Fatalf("a secret left the host: %q", f)
		}
	}
}
