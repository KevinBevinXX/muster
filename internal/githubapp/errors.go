package githubapp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// apiError is a non-2xx answer from GitHub, worded for the OPERATOR: it is what
// the agent card and the connector request card show (App.Problem), and what
// the proxy answers an agent whose request could not be served. It carries
// GitHub's own message (never a token or the JWT, which are only ever in
// request headers).
type apiError struct {
	status int
	msg    string
	// rateLimited: GitHub said the App is over a rate limit.
	rateLimited bool
}

func (e *apiError) Error() string { return e.msg }

// appWide reports a failure that affects every agent, not just one repository:
// the App's credentials refused (401), or a rate limit.
func (e *apiError) appWide() bool { return e.status == http.StatusUnauthorized || e.rateLimited }

func newAPIError(resp *http.Response, raw []byte, op, account string, appID int64, now time.Time) error {
	var body struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &body)
	gh := strings.TrimSpace(body.Message)
	if len(gh) > 200 {
		gh = gh[:200] + "…"
	}
	said := ""
	if gh != "" {
		said = " (GitHub said: " + strconv.Quote(gh) + ")"
	}
	e := &apiError{status: resp.StatusCode}
	remaining := resp.Header.Get("X-RateLimit-Remaining")
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		e.msg = fmt.Sprintf("GitHub refused the GitHub App's credentials (401) while %s%s: MUSTER_GITHUB_APP_ID "+
			"(%d) and MUSTER_GITHUB_APP_PRIVATE_KEY must be the same App's, the key must not have been revoked, and "+
			"this server's clock must be within a minute of GitHub's (the App's JWT is valid from 60 s before "+
			"to 9 min after it is signed)", op, said, appID)
	case (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) &&
		(remaining == "0" || resp.Header.Get("Retry-After") != ""):
		e.rateLimited = true
		when := "later"
		if s := resp.Header.Get("Retry-After"); s != "" {
			when = "in " + s + "s"
		} else if r, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			when = "after " + time.Unix(r, 0).UTC().Format(time.RFC3339) + " (" +
				time.Unix(r, 0).Sub(now).Round(time.Second).String() + ")"
		}
		e.msg = fmt.Sprintf("GitHub rate-limited the GitHub App while %s%s; muster retries on the next request, "+
			"which can succeed %s", op, said, when)
	case resp.StatusCode == http.StatusNotFound && account != "":
		e.msg = fmt.Sprintf("GitHub answered 404 while %s%s: the App's installation on %s is gone or the "+
			"repository does not exist", op, said, account)
	case resp.StatusCode == http.StatusUnprocessableEntity:
		e.msg = fmt.Sprintf("GitHub refused (422) while %s%s: the repository is probably not among those the App's "+
			"installation on %s may access — add it under the installation's Repository access on GitHub", op, said, account)
	default:
		e.msg = fmt.Sprintf("GitHub answered %d while %s%s", resp.StatusCode, op, said)
	}
	return e
}
