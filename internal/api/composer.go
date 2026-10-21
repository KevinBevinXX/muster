package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE MESSAGE COMPOSER's server half, shared by every route a composer posts
// to: a mirrored session's two send routes (mirror_view.go) and an agent's
// (agent_send.go).
//
// 🔴 A SEND ARRIVES ONE OF TWO WAYS, AND EVERY ROUTE ANSWERS BOTH.
//
//   - From the composer's SCRIPT (a fetch): 204, or a status and the reason as
//     plain text, which the script shows inline. A stale sign-in is 403 with
//     X-Muster-Step-Up, which the script answers with the confirm dialog.
//   - From the BROWSER (the form submitted natively — the script did not run,
//     threw, or handed over): a NAVIGATION. It is answered with a redirect back
//     to the view the message was sent from, carrying a notice code the page
//     then draws; a stale sign-in is answered with the plain confirm page
//     (ui.StepUpFormPage), which completes the send once the password is right.
//
// isDocumentNavigation is what tells them apart, and it errs toward the first:
// a request it cannot place gets the status and the text, never a redirect.
// ---------------------------------------------------------------------------

// The notice codes a redirect carries (?notice=…). A code is all that travels:
// the text shown is this server's own, so a link cannot put words on the page.
const (
	noticeSent       = "sent"
	noticeEmpty      = "empty"
	noticeAgain      = "again"
	noticeOffline    = "offline"
	noticeGone       = "gone"
	noticeWindowGone = "window-gone"
	noticeReadOnly   = "read-only"
	noticeNotClaude  = "not-claude"
	noticeUpgrade    = "upgrade"
	noticeBadText    = "bad-text"
	noticeFailed     = "failed"
	noticeNotRunning = "not-running"
	noticeNoGateway  = "no-gateway"
	// The send queue's (send_queue.go).
	noticeQueued       = "queued"
	noticeQueueOff     = "queue-off"
	noticeQueueFull    = "queue-full"
	noticeCancelled    = "cancelled"
	noticeNotCancelled = "not-cancelled"
	// noticeCancelledAnswered: cancelled, but the message had already answered
	// a question in the session (attention.go's askerAddressed) — that answer
	// was delivered and stays.
	noticeCancelledAnswered = "cancelled-answered"
	noticeNotRetried        = "not-retried"
	// noticeRoutedNotRetried: a ROUTED message is not retried as the
	// person's own; its failed route went back to the chief.
	noticeRoutedNotRetried = "routed-not-retried"
	// A finished message's Delete (send_queue.go).
	noticeDeleted    = "deleted"
	noticeNotDeleted = "not-deleted"
	// A queued message's Send now (send_queue_now.go). Sent is noticeSent.
	noticeNotQueuedNow   = "not-queued-now"
	noticeSendingAnother = "sending-another"
	noticeSendNowFailed  = "send-now-failed"
	noticeWaitingNow     = "waiting-now"
	// A Hermes window's Send now refusals.
	noticeHermesWaitingNow = "hermes-waiting-now"
	noticeHermesBusyNow    = "hermes-busy-now"
	noticeHermesUnknownNow = "hermes-unknown-now"
	// A Hermes window's Send (Phase 3): the hub's idle gate, and the host's
	// gated send by how it ended.
	noticeHermesBusy        = "hermes-busy"
	noticeHermesWaiting     = "hermes-waiting"
	noticeHermesUnknown     = "hermes-unknown"
	noticeHermesNotSent     = "hermes-not-sent"
	noticeHermesPasted      = "hermes-pasted"
	noticeHermesUnconfirmed = "hermes-unconfirmed"
	// Any send the host never confirmed, and its repeat with the same nonce.
	noticeUnconfirmed       = "unconfirmed"
	noticeUnconfirmedRepeat = "unconfirmed-repeat"
	// Send now: the message may have landed, or sits pasted in Hermes's box.
	noticeSendNowUnconfirmed = "send-now-unconfirmed"
	noticeSendNowPasted      = "send-now-pasted"
	// The host's Hermes paste guard (mirror.PasteRefusal*).
	noticeHermes      = "hermes"
	noticeMultiplexer = "multiplexer"
	noticeUnchecked   = "unchecked"
	// A queued message's Edit (send_queue_edit.go).
	noticeEditing           = "editing"
	noticeEdited            = "edited"
	noticeEditCancelled     = "edit-cancelled"
	noticeEditLapsed        = "edit-lapsed"
	noticeEditTooLate       = "edit-too-late"
	noticeEditTooLateSent   = "edit-too-late-sent"
	noticeNotEditable       = "not-editable"
	noticeEditSending       = "edit-sending"
	noticeRoutedNotEditable = "routed-not-editable"
	noticeHeldNow           = "held-now"
	// noticeEditedAnswered: saved, but the message had already answered a
	// question in its session when it was queued — that answer stands.
	noticeEditedAnswered = "edited-answered"
)

// pasteGuardNotices is the notice for each kind of Hermes paste-guard refusal
// a host names.
var pasteGuardNotices = map[string]string{
	mirror.PasteRefusalHermes:      noticeHermes,
	mirror.PasteRefusalMultiplexer: noticeMultiplexer,
	mirror.PasteRefusalUnchecked:   noticeUnchecked,
}

// hermesNotIdleNotices is the notice for each status the hub's idle gate
// refuses a Hermes send in ("" unknown).
var hermesNotIdleNotices = map[string]string{
	mirror.StatusBusy:    noticeHermesBusy,
	mirror.StatusWaiting: noticeHermesWaiting,
	"":                   noticeHermesUnknown,
	// idle with no status time: the host could not check it, so unknown.
	mirror.StatusIdle: noticeHermesUnknown,
}

// hermesSendNotices is the notice for each way the host's gated send into
// Hermes can end short of "sent" (mirror.PasteRefusalHermes*).
var hermesSendNotices = map[string]string{
	mirror.PasteRefusalHermesNotSubmitted: noticeHermesNotSent,
	mirror.PasteRefusalHermesSuperseded:   noticeHermesNotSent,
	mirror.PasteRefusalHermesPasted:       noticeHermesPasted,
	mirror.PasteRefusalHermesUnconfirmed:  noticeHermesUnconfirmed,
}

// ComposerNoticeAgain is what a send that came back as a GET is told: it was
// not delivered (its body did not survive the sign-in it was sent through).
const ComposerNoticeAgain = "You were signed in again on the way, so that message was not sent. Send it again."

// AgentNotRunningNotice is why an agent that is not running cannot be sent a
// message.
const AgentNotRunningNotice = "This agent is not running, so it cannot be sent a message. Start it first."

// composerNotices is each code's text, and whether it is good news.
var composerNotices = map[string]struct {
	text string
	ok   bool
}{
	noticeSent:              {"Sent.", true},
	noticeEmpty:             {ui.MirrorComposeEmpty, false},
	noticeAgain:             {ComposerNoticeAgain, false},
	noticeOffline:           {unprefixed(mirror.ErrHostOffline), false},
	noticeGone:              {unprefixed(mirror.ErrSessionGone), false},
	noticeWindowGone:        {unprefixed(mirror.ErrWindowGone), false},
	noticeReadOnly:          {unprefixed(mirror.ErrInteractiveRefused), false},
	noticeNotClaude:         {unprefixed(mirror.ErrNotClaude), false},
	noticeUpgrade:           {unprefixed(mirror.ErrPanePasteUnsupported), false},
	noticeBadText:           {"Not sent: a message is plain text (newlines and tabs only), up to 16 KiB.", false},
	noticeFailed:            {"Not sent: the message could not be delivered.", false},
	noticeNotRunning:        {AgentNotRunningNotice, false},
	noticeNoGateway:         {"Not sent: this server has no agent gateway to deliver a message through.", false},
	noticeQueued:            {"Queued: it is sent when the session has been idle for a while, even if you close this page.", true},
	noticeQueueOff:          {"Not queued: muster cannot tell when this Claude Code session is idle, so it cannot queue for it. Send it now, or upgrade muster-mirror.", false},
	noticeQueueFull:         {"Not queued: " + strings.TrimPrefix(sendqueue.ErrFull.Error(), "sendqueue: ") + ".", false},
	noticeCancelled:         {"Cancelled: that queued message will not be sent.", true},
	noticeNotCancelled:      {"Not cancelled: that message is already being sent, or has been.", false},
	noticeCancelledAnswered: {SendQueueCancelledAnsweredText(""), false},
	noticeNotRetried:        {"Not queued again: only a message that failed can be retried, and only once.", false},
	noticeRoutedNotRetried:  {"Not queued again: your chief routed that message, and a failed route goes back to the chief rather than being retried here.", false},
	noticeDeleted:           {"Deleted.", true},
	noticeNotDeleted:        {SendQueueNotDeleted, false},
	noticeNotQueuedNow:      {SendQueueNotQueuedNow, false},
	noticeSendingAnother:    {SendQueueSendingAnother, false},
	noticeWaitingNow:        {SendQueueWaitingNow, false},
	noticeHermesWaitingNow:  {SendQueueHermesWaitingNow, false},
	noticeHermesBusyNow:     {SendQueueHermesBusyNow, false},
	noticeHermesUnknownNow:  {SendQueueHermesUnknownNow, false},
	noticeSendNowFailed:     {"Not sent: the message could not be delivered. Its row in the queue says why.", false},
	noticeHermesBusy:        {mirror.HermesBusyText, false},
	noticeHermesWaiting:     {mirror.HermesWaitingText, false},
	noticeHermesUnknown:     {mirror.HermesUnknownText, false},
	noticeHermesNotSent:     {"Not sent: Hermes's screen or status was not safe to send into. Nothing was pasted.", false},
	noticeHermesPasted:      {"Pasted, not submitted: the text is in Hermes's input box, and Enter was not pressed. Check the terminal.", false},
	noticeHermesUnconfirmed: {"May have been submitted: check the conversation before sending again.", false},
	noticeUnconfirmed:        {MirrorSendUnconfirmed, false},
	noticeUnconfirmedRepeat:  {MirrorSendUnconfirmedRepeat, false},
	noticeSendNowUnconfirmed: {"May have been sent: check the conversation before sending again. Its row in the queue says more.", false},
	noticeSendNowPasted:      {"Pasted, not submitted: the text is in Hermes's input box. Check the terminal. Its row in the queue says more.", false},
	noticeHermes:            {mirror.PasteRefusedHermesText, false},
	noticeMultiplexer:       {mirror.PasteRefusedMultiplexerText, false},
	noticeUnchecked:         {mirror.PasteRefusedUncheckedText, false},
	noticeEditing:           {SendQueueEditingText, true},
	noticeEdited:            {SendQueueEditedText, true},
	noticeEditCancelled:     {SendQueueEditCancelledText, true},
	noticeEditLapsed:        {SendQueueEditLapsed, false},
	noticeEditTooLate:       {SendQueueEditTooLate, false},
	noticeEditTooLateSent:   {SendQueueEditTooLateSent, false},
	noticeNotEditable:       {SendQueueNotEditable, false},
	noticeEditSending:       {SendQueueEditSending, false},
	noticeRoutedNotEditable: {SendQueueRoutedNotEditable, false},
	noticeHeldNow:           {SendQueueHeldNow, false},
	noticeEditedAnswered:    {SendQueueEditedAnsweredText(""), false},
}

// SendQueueEditedAnsweredText is what a Save of a queued message that had
// already answered asker's question when it was queued says ("" = the asker
// unnamed): the text that will be pasted changed, the answer did not.
// (Cancel's twin: SendQueueCancelledAnsweredText.)
func SendQueueEditedAnsweredText(asker string) string {
	whose := "a question in that session"
	if asker != "" {
		whose = asker + "'s question"
	}
	return "Saved — but this message had already answered " + whose + ", and that answer was delivered as first written: the edit does not change it."
}

// What an edit's own notices say.
var (
	SendQueueEditingText       = "Editing: the message is held, and nothing queued for this window is sent until you Save or Cancel (at most " + editHoldText + ")."
	SendQueueEditedText        = "Saved: the message keeps its place in the queue."
	SendQueueEditCancelledText = "Edit cancelled: the message is unchanged."
)

// SendQueueCancelledAnsweredText is what a Cancel of a queued message that
// had already answered asker's question says ("" = the asker unnamed): the
// queued paste is cancelled, the answer is not.
func SendQueueCancelledAnsweredText(asker string) string {
	whose := "a question in that session"
	if asker != "" {
		whose = asker + "'s question"
	}
	return "Cancelled — but this message had already answered " + whose + ", and that answer was delivered: it can't be un-sent."
}

func unprefixed(err error) string {
	msg := strings.TrimPrefix(err.Error(), "mirror: ")
	return strings.ToUpper(msg[:1]) + msg[1:] + "."
}

// composerNotice is the notice a page was redirected to with: its text and
// whether it is good news. An unknown code is no notice.
func composerNotice(r *http.Request) (text string, ok bool) {
	n, known := composerNotices[r.URL.Query().Get("notice")]
	if !known {
		return "", false
	}
	return n.text, n.ok
}

// keepNotice is path with the request's own notice carried over: a view that
// answers with a further redirect (to a window's explicit address, to the
// Terminal when there is no rendered view) must not lose what the first
// redirect was telling the person.
func keepNotice(r *http.Request, path string) string {
	code := r.URL.Query().Get("notice")
	if _, known := composerNotices[code]; !known {
		return path
	}
	return withNotice(path, code)
}

// withNotice is path with ?notice=code added.
func withNotice(path, code string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "notice=" + code
}

// noStore marks a document as never to be kept: not in the HTTP cache, and so
// not in the back/forward cache either.
//
// 🔴 A VIEW'S DOCUMENT IS ITS MARKUP AND ITS INLINE SCRIPT TOGETHER, bound to
// the routes of the build that rendered it. Kept across a deploy and shown
// again (the back button, an installed app resuming a page), it would run one
// build's script against another build's server. The service worker never
// stores these documents (web/static/sw.js caches the shell, the stylesheet
// and the manifest, and nothing else); this is the same statement to the
// browser's own caches.
func noStore(w http.ResponseWriter) { w.Header().Set("Cache-Control", "no-store") }

// formDocument marks a page that carries one of the forms a browser may submit
// BY ITSELF (a composer, the chat form, the plain confirm page): never stored,
// and served `Referrer-Policy: same-origin` in place of securityHeaders'
// no-referrer.
//
// 🔴 WITHOUT THE SECOND HEADER THE FORM'S OWN SUBMIT IS REFUSED. Under
// no-referrer a browser sends `Origin: null` on a form POST navigation (fetch
// from the same page still sends the real origin), and requireSameOrigin
// refuses a null origin — so the fallback this change exists for answered 403
// "cross-origin request refused" to muster's own page. Measured in Chromium by
// e2e/tests/agent-composer.spec.ts; login.go's pages carry the same override
// for the same reason. same-origin still sends nothing to any other site, and
// none of these pages has a secret in its URL.
func formDocument(w http.ResponseWriter) {
	noStore(w)
	w.Header().Set("Referrer-Policy", "same-origin")
}

// draftUser is the user a message box's draft key is made for
// (ui.MirrorDraftKey and the rest): the request's own principal.
func draftUser(ctx context.Context) int64 {
	p, _ := authz.From(ctx)
	return p.UserID
}

// newSendNonce mints a composer's nonce (ui.ComposerView.Nonce).
func newSendNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "" // no nonce is no protection, never a refusal
	}
	return hex.EncodeToString(b[:])
}

var sendNoncePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

const (
	// sendNonceTTL is how long a delivered message's nonce is remembered: longer
	// than any retry of the same send takes, short enough to be a small table.
	sendNonceTTL = 15 * time.Minute
	// sendNonceMax bounds the table; past it the oldest entries go first.
	sendNonceMax = 4096
)

// sendNonces remembers which messages were delivered, so that the SAME message
// posted twice — a double tap, or the composer's script handing a send whose
// answer was lost to the browser's own submit — is delivered once.
//
// 🔴 A MESSAGE IS ITS OWNER, ITS NONCE AND ITS TEXT. The nonce alone is not
// enough: a page whose script failed to renew it would have its NEXT message
// taken for a repeat and silently dropped, which is the worst failure a
// message box has. With the text in the key, only the same words under the
// same nonce are a repeat.
//
// In memory, per process: a restart forgets, and the cost of forgetting is one
// possible duplicate, not a lost message. The zero value is ready to use.
type sendNonces struct {
	mu   sync.Mutex
	seen map[string]sendNonce
	now  func() time.Time
}

// sendNonce is one claimed message: when it was claimed, or delivered.
type sendNonce struct {
	at time.Time
	// unconfirmed: handed to the host, never confirmed (claim).
	unconfirmed bool
}

func (n *sendNonces) clock() time.Time {
	if n.now != nil {
		return n.now()
	}
	return time.Now()
}

func sendNonceKey(owner int64, nonce, text string) string {
	if !sendNoncePattern.MatchString(nonce) {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return strconv.FormatInt(owner, 10) + "|" + nonce + "|" + hex.EncodeToString(sum[:])
}

// nonceState is what a claim found (claim).
type nonceState int

const (
	// nonceNew: not seen — deliver it.
	nonceNew nonceState = iota
	// nonceRepeat: delivered, or being delivered now — answer "sent".
	nonceRepeat
	// nonceUnconfirmed: handed to the host and never confirmed — it may be
	// in the conversation. 🔴 NEVER DELIVERED AGAIN under this nonce: a retry
	// is the person's explicit "Send again", which mints a new one.
	nonceUnconfirmed
)

// nonceResult is how a claimed delivery ended (claim's finish).
type nonceResult int

const (
	nonceFailed nonceResult = iota // not delivered: the claim is released
	nonceDelivered
	nonceMaybe // the host never confirmed: kept, as unconfirmed
)

// claim is begin with the states a mirrored send needs: a repeat of an
// UNCONFIRMED delivery is told apart from a repeat of a delivered one.
func (n *sendNonces) claim(owner int64, nonce, text string) (nonceState, func(nonceResult)) {
	key := sendNonceKey(owner, nonce, text)
	if key == "" {
		return nonceNew, func(nonceResult) {}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.clock()
	if n.seen == nil {
		n.seen = map[string]sendNonce{}
	}
	if e, ok := n.seen[key]; ok && now.Sub(e.at) < sendNonceTTL {
		if e.unconfirmed {
			return nonceUnconfirmed, func(nonceResult) {}
		}
		return nonceRepeat, func(nonceResult) {}
	}
	n.evict(now)
	n.seen[key] = sendNonce{at: now}
	return nonceNew, func(r nonceResult) {
		n.mu.Lock()
		defer n.mu.Unlock()
		switch r {
		case nonceFailed:
			delete(n.seen, key)
		case nonceMaybe:
			n.seen[key] = sendNonce{at: n.clock(), unconfirmed: true}
		default:
			n.seen[key] = sendNonce{at: n.clock()}
		}
	}
}

// begin claims a message for delivery. repeat is true when the same message
// was already delivered, or is being delivered right now: the caller answers
// "sent" and delivers nothing. Otherwise the caller delivers it and calls
// finish with whether that worked — a failed delivery releases the claim, so
// the same message may be tried again.
//
// A request with no usable nonce (an older page, a hand-made request) is never
// a repeat and claims nothing.
func (n *sendNonces) begin(owner int64, nonce, text string) (repeat bool, finish func(delivered bool)) {
	st, fin := n.claim(owner, nonce, text)
	return st != nonceNew, func(delivered bool) {
		if delivered {
			fin(nonceDelivered)
		} else {
			fin(nonceFailed)
		}
	}
}

// evict makes room for one more claim: expired ones go, and at the cap the
// oldest. n.mu is held.
func (n *sendNonces) evict(now time.Time) {
	if len(n.seen) < sendNonceMax {
		return
	}
	oldest, oldestAt := "", now
	for k, e := range n.seen {
		if now.Sub(e.at) >= sendNonceTTL {
			delete(n.seen, k)
		} else if !e.at.After(oldestAt) {
			oldest, oldestAt = k, e.at
		}
	}
	if len(n.seen) >= sendNonceMax && oldest != "" {
		delete(n.seen, oldest)
	}
}
