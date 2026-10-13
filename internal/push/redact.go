package push

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
)

// Short renders an endpoint for a log line or a page: the push service's host
// and a fingerprint, never the path.
//
// 🔴 A PUSH ENDPOINT IS A CAPABILITY, NOT A LONG STRING. Anyone holding one can
// make that device buzz with no further credential. The router's version kept
// the first 60 characters, which for an FCM endpoint
// (`https://fcm.googleapis.com/fcm/send/` is 36) is 24 characters of the
// device's token in every log line. The path is where the capability lives, so
// none of it is shown; the fingerprint (8 hex of its sha256) is enough to tell
// two devices apart in a log and correlate one across lines.
func Short(endpoint string) string {
	host := "?"
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		host = u.Host
	}
	return host + "/…#" + Fingerprint(endpoint)
}

// Fingerprint is 8 hex characters of the endpoint's sha256: an identifier for
// logs and pages that does not reveal the endpoint.
func Fingerprint(endpoint string) string {
	return EndpointHash(endpoint)[:8]
}

// EndpointHash is the full hex sha256 of an endpoint. It is what the browser's
// device cookie carries (see api's push routes): enough to name this browser's
// subscription at logout without the cookie holding the capability itself.
func EndpointHash(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(sum[:])
}

// Redact renders err for a log line with every occurrence of endpoint replaced
// by its Short form.
//
// 🔴 TRUNCATING THE ENDPOINT IN ONE Printf ARGUMENT DOES NOTHING IF THE NEXT
// ARGUMENT IS AN ERROR THAT EMBEDS IT. webpush-go returns http.Client.Do's error
// unwrapped; for a transport failure that is a *url.Error, whose Error() is
// `Post "<the whole endpoint>": …`. So every log line that interpolates an
// error goes through here, and it scrubs the STRING that reaches the output —
// agnostic to the error's type (a wrapped *url.Error, a driver error quoting a
// bind parameter). *url.Error prints the RE-SERIALISED URL, which can differ
// from the raw endpoint (escaping, normalisation), so that form is scrubbed too.
func Redact(err error, endpoint string) string {
	if err == nil {
		return "<nil>"
	}
	msg := err.Error()
	if endpoint == "" {
		return msg
	}
	short := Short(endpoint)
	msg = strings.ReplaceAll(msg, endpoint, short)
	if u, perr := url.Parse(endpoint); perr == nil {
		if s := u.String(); s != "" && s != endpoint {
			msg = strings.ReplaceAll(msg, s, short)
		}
		// The path alone (a driver error can quote a substring, and a proxy can
		// echo just the request path).
		if u.Path != "" && len(u.Path) > 8 {
			msg = strings.ReplaceAll(msg, u.Path, "/…#"+Fingerprint(endpoint))
		}
	}
	return msg
}
