package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"html"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// --- accounts PR 5: the admin pages ------------------------------------------
//
// /admin/users and /admin/invites: the one place an admin acts on OTHER users.
// Every route is requireAdmin — a session whose principal's role is admin —
// and every write leaves an audit_log row naming who did it.
//
// 🔴 "ADMIN SEES ALL" EXISTS ONLY HERE. An admin's ordinary routes are scoped
// to its own rows by row-level security like anyone's; the store calls behind
// these pages run as authz.System (internal/users), so the ROLE CHECK BELOW IS
// THE WHOLE GATE. The isolation walk classifies each route "admin-only" and
// asserts a member is refused every one, with the admin's own session as the
// control (TestAMemberCannotReachAnyAdminRoute).

// AccountStore is what invites and the admin pages need from internal/users.
type AccountStore interface {
	CreateInvite(ctx context.Context, by users.Actor, tokenHash []byte, email string, expiresAt time.Time) (users.Invite, error)
	ListInvites(ctx context.Context) ([]users.Invite, error)
	RevokeInvite(ctx context.Context, by users.Actor, id int64) error
	OpenInvite(ctx context.Context, tokenHash []byte) (users.Invite, error)
	RedeemInvite(ctx context.Context, tokenHash []byte, email, displayName string,
		create func(context.Context) (string, error), undo func(sub string)) (users.User, error)
	ListUsers(ctx context.Context) ([]users.User, error)
	SetDisabledAs(ctx context.Context, by *users.Actor, id int64, disabled bool) (users.User, error)
	ResetCredentials(ctx context.Context, id int64) (time.Time, error)
	SetRole(ctx context.Context, by users.Actor, id int64, role authz.Role) (users.User, error)
	Audit(ctx context.Context, by users.Actor, action, object string, detail map[string]any) error
	Audited(ctx context.Context, by users.Actor, action, object string, detail map[string]any, fn func() error) error
	RecentAudit(ctx context.Context, limit int) ([]users.AuditEntry, error)
}

// InviteTTL is an invite link's default lifetime; inviteTTLChoices are the
// lifetimes the create form offers.
const InviteTTL = 72 * time.Hour

var inviteTTLChoices = []int{24, 72, 168}

// InvitePath is the invitee's page. The token rides in the URL FRAGMENT
// (`/invite#<token>`), for the reason ClaimPath gives.
const InvitePath = "/invite"

// ResetPath is the password-reset page; its token rides in the fragment too.
const ResetPath = "/auth/reset"

// registerAdminRoutes wires the admin pages. Literal patterns, for the reason
// registerLoginRoutes gives.
func (s *Server) registerAdminRoutes(mux Mux) {
	mux.HandleFunc("GET /admin/users", s.requireAdmin(s.handleAdminUsers))
	mux.HandleFunc("POST /admin/users/{id}/disable", s.requireAdmin(s.handleAdminUserDisable))
	mux.HandleFunc("POST /admin/users/{id}/enable", s.requireAdmin(s.handleAdminUserEnable))
	mux.HandleFunc("POST /admin/users/{id}/role", s.requireAdmin(s.handleAdminUserRole))
	mux.HandleFunc("POST /admin/users/{id}/reset-link", s.requireAdmin(s.handleAdminUserResetLink))
	mux.HandleFunc("POST /admin/users/{id}/passkeys/delete", s.requireAdmin(s.handleAdminUserPasskeysDelete))
	mux.HandleFunc("GET /admin/invites", s.requireAdmin(s.handleAdminInvites))
	mux.HandleFunc("POST /admin/invites", s.requireAdmin(s.handleAdminInviteCreate))
	mux.HandleFunc("POST /admin/invites/{id}/revoke", s.requireAdmin(s.handleAdminInviteRevoke))
}

// adminPagesServed reports whether this server has what the admin pages need.
func (s *Server) adminPagesServed() bool { return s.gt != nil && s.gt.Accounts != nil }

// AdminOnlyRefusal is what a non-admin principal gets from /admin/*.
const AdminOnlyRefusal = "this page is for admins"

// requireAdmin is requireSession plus: the principal's role is admin, the
// admin store is wired, and a write comes from muster's own pages.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireSession(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := authz.From(r.Context()); !ok || p.Role != authz.RoleAdmin {
			if isDocumentNavigation(r) {
				s.renderNotice(w, http.StatusForbidden, "Admins only", "Your account cannot open this page.")
				return
			}
			s.writeJSON(w, http.StatusForbidden, map[string]any{"error": AdminOnlyRefusal})
			return
		}
		if !s.adminPagesServed() {
			s.renderNotice(w, http.StatusServiceUnavailable, "Account management is not configured",
				"This server has no GoTrue configured (MUSTER_GOTRUE_URL), so it cannot manage accounts.")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.isSameOrigin(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]any{"error": CrossOriginRefusal})
			return
		}
		next(w, r)
	})
}

func actorOf(r *http.Request) users.Actor {
	p := authz.MustFrom(r.Context())
	return users.Actor{UserID: p.UserID, Via: p.Via}
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// --- layout -------------------------------------------------------------------

const (
	adminCard   = `flex flex-col gap-3 rounded-2xl border border-line bg-s1/70 px-4 py-6 ring-1 ring-line`
	adminInput  = `min-h-[44px] rounded-md bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus`
	adminButton = `press min-h-[44px] rounded-md bg-accent px-3 py-2 text-base font-semibold text-on-accent transition hover:bg-accent/90`
	adminQuiet  = `press min-h-[44px] rounded-md bg-bg px-3 py-2 text-sm font-semibold text-fg ring-1 ring-inset ring-edge`
)

// writeAdminDocument is writeAuthDocument with the admin header.
func writeAdminDocument(w http.ResponseWriter, code int, title, active, inner string) {
	tab := func(href, label string) string {
		cls := "inline-flex min-h-[44px] items-center rounded-md px-3 text-sm font-medium text-fg2 hover:bg-s2"
		cur := ""
		if href == active {
			cls = "inline-flex min-h-[44px] items-center rounded-md bg-accent/15 px-3 text-sm font-medium text-fg ring-1 ring-inset ring-accent/30"
			cur = ` aria-current="page"`
		}
		return `<a href="` + href + `"` + cur + ` class="` + cls + `">` + label + `</a>`
	}
	writeAuthDocument(w, code, title, `<nav aria-label="Admin" class="mb-4 flex flex-wrap items-center gap-1">
<a href="/" class="inline-flex min-h-[44px] items-center px-2 text-sm text-accent underline">← muster</a>
`+tab("/admin/users", "Users")+tab("/admin/invites", "Invites")+tab("/account", "Account")+`
</nav>
<div class="flex flex-col gap-4">`+inner+`</div>`)
}

// shownOnce renders a link that will never be shown again.
func shownOnce(heading, link, note string) string {
	return `<section class="` + adminCard + `" aria-labelledby="once-h">
<h2 id="once-h" class="text-base font-semibold text-fg">` + html.EscapeString(heading) + `</h2>
<p class="text-sm text-fg2">` + html.EscapeString(note) + ` It is shown only this once.</p>
<input id="once-link" readonly value="` + html.EscapeString(link) + `" class="` + adminInput + ` w-full font-mono text-sm" onclick="this.select()">
</section>`
}

func fmtTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

// --- /admin/users ---------------------------------------------------------------

func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	s.renderAdminUsers(w, r, http.StatusOK, "", "")
}

func (s *Server) renderAdminUsers(w http.ResponseWriter, r *http.Request, code int, problem, once string) {
	ctx := r.Context()
	list, err := s.gt.Accounts.ListUsers(ctx)
	if err != nil {
		s.logger.Printf("admin: list users: %v", err)
		s.renderNotice(w, http.StatusInternalServerError, "Could not load users", "Try again in a moment.")
		return
	}
	me := authz.MustFrom(ctx).UserID
	passkeys := s.passkeysServed(ctx)
	var b strings.Builder
	b.WriteString(once)
	b.WriteString(problemNode(problem))
	b.WriteString(`<section class="` + adminCard + `"><h1 class="text-base font-semibold text-fg">Users</h1><ul class="flex flex-col gap-3" id="admin-users">`)
	for _, u := range list {
		id := strconv.FormatInt(u.ID, 10)
		// The role shown is roleOf's — the ONE role read every principal uses
		// (users.User deliberately carries none; see internal/users).
		role := s.roleOf(ctx, u.ID)
		state := "active"
		switch {
		case u.Disabled():
			state = "disabled"
		case u.GotrueSub == "":
			state = "no login yet"
		}
		name := u.Email
		if name == "" {
			name = u.DisplayName
		}
		b.WriteString(`<li class="flex flex-col gap-2 rounded-lg border border-line px-3 py-3" data-user-id="` + id + `" data-user-email="` + html.EscapeString(u.Email) + `">
<div class="flex flex-wrap items-baseline gap-2"><span class="font-semibold text-fg">` + html.EscapeString(name) + `</span>
<span class="text-xs text-fg2">#` + id + ` · <span data-user-role>` + html.EscapeString(string(role)) + `</span> · <span data-user-state>` + state + `</span></span></div>`)
		if u.DisplayName != "" && u.Email != "" {
			b.WriteString(`<p class="text-sm text-fg2">` + html.EscapeString(u.DisplayName) + `</p>`)
		}
		b.WriteString(`<div class="flex flex-wrap gap-2">`)
		if u.ID != me {
			if u.Disabled() {
				b.WriteString(`<form method="POST" action="/admin/users/` + id + `/enable"><button type="submit" class="` + adminQuiet + `">Enable</button></form>`)
			} else {
				b.WriteString(`<form method="POST" action="/admin/users/` + id + `/disable"><button type="submit" class="` + adminQuiet + `">Disable</button></form>`)
			}
		}
		other := authz.RoleAdmin
		if role == authz.RoleAdmin {
			other = authz.RoleMember
		}
		b.WriteString(`<form method="POST" action="/admin/users/` + id + `/role"><input type="hidden" name="role" value="` + string(other) + `">
<button type="submit" class="` + adminQuiet + `">Make ` + string(other) + `</button></form>`)
		if u.GotrueSub != "" && u.Email != "" {
			b.WriteString(`<form method="POST" action="/admin/users/` + id + `/reset-link"><button type="submit" class="` + adminQuiet + `">Reset link</button></form>`)
		}
		if u.GotrueSub != "" && passkeys {
			b.WriteString(`<form method="POST" action="/admin/users/` + id + `/passkeys/delete"><button type="submit" class="` + adminQuiet + `">Remove passkeys</button></form>`)
		}
		b.WriteString(`</div></li>`)
	}
	b.WriteString(`</ul></section>`)
	if entries, err := s.gt.Accounts.RecentAudit(ctx, 25); err == nil && len(entries) > 0 {
		b.WriteString(`<section class="` + adminCard + `"><h2 class="text-base font-semibold text-fg">Recent account activity</h2><ul class="flex flex-col gap-1 text-sm text-fg2" id="admin-audit">`)
		for _, e := range entries {
			b.WriteString(`<li><span class="text-fg">` + html.EscapeString(e.Action) + `</span> ` + html.EscapeString(e.Object) +
				` · by user ` + strconv.FormatInt(e.UserID, 10) + ` (` + html.EscapeString(e.Via) + `) · ` + fmtTime(e.At) + `</li>`)
		}
		b.WriteString(`</ul></section>`)
	}
	writeAdminDocument(w, code, "Users", "/admin/users", b.String())
}

// adminUserTarget reads {id} and refuses acting on an unknown user.
func (s *Server) adminUserTarget(w http.ResponseWriter, r *http.Request) (users.User, bool) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return users.User{}, false
	}
	u, err := s.gt.Users.ByID(r.Context(), id)
	if errors.Is(err, users.ErrNotFound) {
		http.NotFound(w, r)
		return users.User{}, false
	}
	if err != nil {
		s.logger.Printf("admin: read user %d: %v", id, err)
		s.renderNotice(w, http.StatusInternalServerError, "Could not load that user", "Try again in a moment.")
		return users.User{}, false
	}
	return u, true
}

func (s *Server) handleAdminUserDisable(w http.ResponseWriter, r *http.Request) {
	s.adminSetDisabled(w, r, true)
}

func (s *Server) handleAdminUserEnable(w http.ResponseWriter, r *http.Request) {
	s.adminSetDisabled(w, r, false)
}

// adminSetDisabled disables or enables a user through DisableUserAs — the
// same path the CLI takes: users.disabled_at (and, on disable, the session
// cutoff), the audit row IN THE SAME TRANSACTION, the GoTrue ban, and (on
// disable) the END of every stream the user has open, so the user's next
// request and every open SSE/terminal/chat stream is refused
// (TestDisablingAMemberEndsItsStreams).
func (s *Server) adminSetDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	u, ok := s.adminUserTarget(w, r)
	if !ok {
		return
	}
	by := actorOf(r)
	if disabled && u.ID == by.UserID {
		s.renderAdminUsers(w, r, http.StatusConflict, "You cannot disable your own account.", "")
		return
	}
	err := s.DisableUserAs(r.Context(), by, u.ID, disabled)
	if errors.Is(err, users.ErrLastAdmin) {
		s.renderAdminUsers(w, r, http.StatusConflict, lastAdminRefusal, "")
		return
	}
	if err != nil {
		// The row change (and its audit row) committed or did not, together;
		// re-read to tell which. A committed change whose GoTrue ban failed is
		// reported, and SetUserDisabledAs has recorded user.ban_failed.
		if cur, rerr := s.gt.Users.ByID(r.Context(), u.ID); rerr != nil || cur.Disabled() != disabled {
			s.logger.Printf("admin: disable=%v user %d: %v", disabled, u.ID, err)
			s.renderAdminUsers(w, r, http.StatusInternalServerError, "That change could not be made. Try again.", "")
			return
		}
		s.renderAdminUsers(w, r, http.StatusBadGateway, err.Error(), "")
		return
	}
	s.logger.Printf("admin: user %d disabled=%v by user %d", u.ID, disabled, by.UserID)
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// lastAdminRefusal is the page's answer to ErrLastAdmin.
const lastAdminRefusal = "That is the last enabled admin; make another admin first."

func (s *Server) handleAdminUserRole(w http.ResponseWriter, r *http.Request) {
	u, ok := s.adminUserTarget(w, r)
	if !ok {
		return
	}
	role := authz.Role(r.FormValue("role"))
	if role != authz.RoleAdmin && role != authz.RoleMember {
		s.renderAdminUsers(w, r, http.StatusBadRequest, "Choose admin or member.", "")
		return
	}
	by := actorOf(r)
	_, err := s.gt.Accounts.SetRole(r.Context(), by, u.ID, role)
	if errors.Is(err, users.ErrLastAdmin) {
		s.renderAdminUsers(w, r, http.StatusConflict, lastAdminRefusal, "")
		return
	}
	if err != nil {
		s.logger.Printf("admin: role of user %d: %v", u.ID, err)
		s.renderAdminUsers(w, r, http.StatusInternalServerError, "The role could not be changed. Try again.", "")
		return
	}
	// No cache to drop: every principal's role is read by roleOf, per
	// request, so the change applies on the user's next request
	// (TestTheLastAdminCannotBeDemotedOrDisabled).
	s.logger.Printf("admin: user %d is now %s (by user %d)", u.ID, role, by.UserID)
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// handleAdminUserResetLink mints a password-reset link for a user: GoTrue's
// recovery token, hashed, in the fragment of <public>/auth/reset. Shown once.
func (s *Server) handleAdminUserResetLink(w http.ResponseWriter, r *http.Request) {
	u, ok := s.adminUserTarget(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(s.auth.PublicURL) == "" {
		s.renderAdminUsers(w, r, http.StatusServiceUnavailable, "MUSTER_PUBLIC_URL is not set, so there is no address to put in the link.", "")
		return
	}
	if u.GotrueSub == "" || u.Email == "" {
		s.renderAdminUsers(w, r, http.StatusConflict, "That user has no login to reset.", "")
		return
	}
	by := actorOf(r)
	var hashed string
	// The audit row and the link are one fact: the row commits only if GoTrue
	// made the link, and the link is shown only if the row committed.
	err := s.gt.Accounts.Audited(r.Context(), by, "user.reset_link", users.UserObject(u.ID), nil, func() error {
		var err error
		hashed, err = s.gt.Client.AdminRecoveryLink(r.Context(), clientIP(r), u.Email)
		return err
	})
	if err != nil {
		s.logger.Printf("admin: reset link for user %d: %v", u.ID, err)
		s.renderAdminUsers(w, r, http.StatusBadGateway, "A reset link could not be made right now.", "")
		return
	}
	link := strings.TrimRight(s.auth.PublicURL, "/") + ResetPath + "#" + hashed
	s.renderAdminUsers(w, r, http.StatusOK, "", shownOnce("Reset link for "+u.Email, link,
		"Send this to them. It sets a new password once and then stops working."))
}

// --- /admin/invites -------------------------------------------------------------

func (s *Server) handleAdminInvites(w http.ResponseWriter, r *http.Request) {
	s.renderAdminInvites(w, r, http.StatusOK, "", "")
}

func (s *Server) renderAdminInvites(w http.ResponseWriter, r *http.Request, code int, problem, once string) {
	ctx := r.Context()
	list, err := s.gt.Accounts.ListInvites(ctx)
	if err != nil {
		s.logger.Printf("admin: list invites: %v", err)
		s.renderNotice(w, http.StatusInternalServerError, "Could not load invites", "Try again in a moment.")
		return
	}
	var b strings.Builder
	b.WriteString(once)
	b.WriteString(problemNode(problem))
	b.WriteString(`<form method="POST" action="/admin/invites" class="` + adminCard + `">
<h1 class="text-base font-semibold text-fg">New invite link</h1>
<label for="invite-email" class="text-sm font-medium text-fg">Email (optional — binds the link to it)</label>
<input id="invite-email" name="email" type="email" autocomplete="off" class="` + adminInput + `">
<p class="text-sm text-fg2">The new account is a <span class="font-semibold text-fg">member</span>. To make an admin, promote them on Users after they join.</p>
<label for="invite-ttl" class="text-sm font-medium text-fg">Expires after</label>
<select id="invite-ttl" name="expires_hours" class="` + adminInput + `">`)
	for _, h := range inviteTTLChoices {
		sel := ""
		if time.Duration(h)*time.Hour == InviteTTL {
			sel = " selected"
		}
		b.WriteString(`<option value="` + strconv.Itoa(h) + `"` + sel + `>` + strconv.Itoa(h) + ` hours</option>`)
	}
	b.WriteString(`</select><button type="submit" class="` + adminButton + `">Create link</button></form>`)
	b.WriteString(`<section class="` + adminCard + `"><h2 class="text-base font-semibold text-fg">Invites</h2><ul class="flex flex-col gap-3" id="admin-invites">`)
	now := s.now()
	if len(list) == 0 {
		b.WriteString(`<li class="text-sm text-fg2">No invites yet.</li>`)
	}
	for _, inv := range list {
		id := strconv.FormatInt(inv.ID, 10)
		st := inv.State(now)
		who := inv.Email
		if who == "" {
			who = "anyone with the link"
		}
		b.WriteString(`<li class="flex flex-col gap-1 rounded-lg border border-line px-3 py-3" data-invite-id="` + id + `" data-invite-state="` + string(st) + `">
<div class="flex flex-wrap items-baseline gap-2"><span class="font-semibold text-fg">` + html.EscapeString(who) + `</span>
<span class="text-xs text-fg2">` + html.EscapeString(string(inv.Role)) + ` · ` + string(st) + `</span></div>
<p class="text-xs text-fg2">created ` + fmtTime(inv.CreatedAt) + ` by ` + html.EscapeString(inv.CreatedByEmail) + ` · expires ` + fmtTime(inv.ExpiresAt) + `</p>`)
		if st == users.InviteRedeemed {
			b.WriteString(`<p class="text-xs text-fg2">redeemed by ` + html.EscapeString(inv.RedeemedByEmail) + `</p>`)
		}
		if st == users.InvitePending {
			b.WriteString(`<form method="POST" action="/admin/invites/` + id + `/revoke"><button type="submit" class="` + adminQuiet + `">Revoke</button></form>`)
		}
		b.WriteString(`</li>`)
	}
	b.WriteString(`</ul></section>`)
	writeAdminDocument(w, code, "Invites", "/admin/invites", b.String())
}

// handleAdminInviteCreate mints an invite: 32 random bytes, only their sha256
// stored, the link `<public>/invite#<token>` shown ONCE in this response.
func (s *Server) handleAdminInviteCreate(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(s.auth.PublicURL) == "" {
		s.renderAdminInvites(w, r, http.StatusServiceUnavailable, "MUSTER_PUBLIC_URL is not set, so there is no address to put in the link.", "")
		return
	}
	email := strings.TrimSpace(r.FormValue("email"))
	if email != "" {
		if a, err := mail.ParseAddress(email); err != nil || a.Address != email {
			s.renderAdminInvites(w, r, http.StatusBadRequest, "Enter a valid email address, or leave it empty.", "")
			return
		}
	}
	ttl := InviteTTL
	if v := r.FormValue("expires_hours"); v != "" {
		h, err := strconv.Atoi(v)
		if err != nil || h < 1 || h > 30*24 {
			s.renderAdminInvites(w, r, http.StatusBadRequest, "Choose an expiry between 1 hour and 30 days.", "")
			return
		}
		ttl = time.Duration(h) * time.Hour
	}
	token, err := auth.RandomToken(32)
	if err != nil {
		s.renderAdminInvites(w, r, http.StatusInternalServerError, "Could not make a token. Try again.", "")
		return
	}
	sum := sha256.Sum256([]byte(token))
	by := actorOf(r)
	inv, err := s.gt.Accounts.CreateInvite(r.Context(), by, sum[:], email, s.now().Add(ttl))
	if errors.Is(err, users.ErrEmailTaken) {
		s.renderAdminInvites(w, r, http.StatusConflict, "A muster account already uses that email.", "")
		return
	}
	if err != nil {
		s.logger.Printf("admin: create invite: %v", err)
		s.renderAdminInvites(w, r, http.StatusInternalServerError, "The invite could not be created. Try again.", "")
		return
	}
	s.logger.Printf("admin: invite %d created by user %d", inv.ID, by.UserID)
	link := strings.TrimRight(s.auth.PublicURL, "/") + InvitePath + "#" + token
	s.renderAdminInvites(w, r, http.StatusOK, "", shownOnce("Invite link", link,
		"Send this link to the person you are inviting. It works once."))
}

func (s *Server) handleAdminInviteRevoke(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	by := actorOf(r)
	err := s.gt.Accounts.RevokeInvite(r.Context(), by, id)
	if errors.Is(err, users.ErrNotRevocable) {
		s.renderAdminInvites(w, r, http.StatusConflict, "That invite is not pending any more.", "")
		return
	}
	if err != nil {
		s.logger.Printf("admin: revoke invite %d: %v", id, err)
		s.renderAdminInvites(w, r, http.StatusInternalServerError, "The invite could not be revoked. Try again.", "")
		return
	}
	s.logger.Printf("admin: invite %d revoked by user %d", id, by.UserID)
	http.Redirect(w, r, "/admin/invites", http.StatusSeeOther)
}
