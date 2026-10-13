package gotruetest

// The fake's /passkeys API, modelled on GoTrue v2.197.0's
// (internal/api/passkey_*.go there): options mint a single-use challenge;
// verify consumes it.
//
// ⚠ IT DOES NO CRYPTOGRAPHY. A credential "verifies" when its `id` names a
// registered passkey (sign-in) or is new (registration) — the fake cannot check
// a signature, and does not try. What it models is everything AROUND that:
// challenge single-use and binding, the banned-user refusal (403 user_banned
// on sign-in and on every user-token call), ownership of a passkey, the admin
// list/delete, the session a sign-in issues (amr "passkey"), and
// passkeys_enabled. The real cryptography is exercised by the e2e spec, which
// drives Chromium's virtual authenticator against the real image.
// LastCredential returns the credential JSON of the most recent verify call,
// so a test can assert what muster forwarded.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/gotrue"
)

// FakePasskey is one registered passkey.
type FakePasskey struct {
	ID           string
	CredentialID string
	User         string
	CreatedAt    time.Time
	LastUsedAt   *time.Time
}

type challenge struct {
	registration bool
	user         string // registration only
	expires      time.Time
}

// EnablePasskeys turns GOTRUE_PASSKEY_ENABLED on (off by default, as in GoTrue).
func (f *Fake) EnablePasskeys() {
	f.mu.Lock()
	f.passkeysEnabled = true
	f.mu.Unlock()
}

// AddPasskey registers a passkey with credential id credID for user id
// without going through HTTP, and returns the passkey's id.
func (f *Fake) AddPasskey(userID, credID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := newUUID()
	f.passkeys[id] = &FakePasskey{ID: id, CredentialID: credID, User: userID, CreatedAt: f.nowLocked()}
	return id
}

// NewChallenge mints a challenge without going through HTTP, as an options
// call would: an authentication one for registrationFor "", else a
// registration one bound to that user.
func (f *Fake) NewChallenge(registrationFor string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, _ := f.newChallengeLocked(registrationFor != "", registrationFor)
	return id
}

// FailAdminPasskeys makes the next n admin passkey calls answer 500 — a
// GoTrue outage in the middle of a list-then-delete.
func (f *Fake) FailAdminPasskeys(n int) {
	f.mu.Lock()
	f.adminPasskeyFailures = n
	f.mu.Unlock()
}

// Passkeys returns user id's passkeys.
func (f *Fake) Passkeys(userID string) []FakePasskey {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []FakePasskey
	for _, p := range f.passkeys {
		if p.User == userID {
			out = append(out, *p)
		}
	}
	return out
}

// LastCredential is the credential JSON the last verify call received.
func (f *Fake) LastCredential() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.lastCredential...)
}

// userFromBearer resolves a user token the way GoTrue's requireAuthentication
// does: a valid JWT, a live session, and — v2.197.0 — a user who is not banned.
func (f *Fake) userFromBearerLocked(w http.ResponseWriter, r *http.Request) (*User, bool) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	v := gotrue.Verifier{Secret: f.Secret, Issuer: Issuer, Audience: Audience, Now: func() time.Time { return f.nowLocked() }}
	c, err := v.Verify(tok)
	if err != nil {
		writeErr(w, 401, "bad_jwt", "invalid JWT")
		return nil, false
	}
	s := f.sessions[c.SessionID]
	if s == nil || s.revoked {
		writeErr(w, 403, "session_not_found", "Session not found")
		return nil, false
	}
	u := f.users[c.Subject]
	if u == nil {
		writeErr(w, 403, "user_not_found", "User not found")
		return nil, false
	}
	if u.Banned {
		writeErr(w, 403, "user_banned", "User is banned")
		return nil, false
	}
	return u, true
}

func (f *Fake) newChallengeLocked(reg bool, user string) (string, string) {
	id := newUUID()
	f.challenges[id] = &challenge{registration: reg, user: user, expires: f.nowLocked().Add(5 * time.Minute)}
	return id, randHex(16)
}

func (f *Fake) takeChallengeLocked(id string, reg bool, user string) bool {
	c, ok := f.challenges[id]
	if !ok || c.registration != reg || c.user != user {
		return false
	}
	delete(f.challenges, id)
	return f.nowLocked().Before(c.expires)
}

func passkeyItem(p *FakePasskey) map[string]any {
	m := map[string]any{"id": p.ID, "friendly_name": "Passkey", "created_at": p.CreatedAt}
	if p.LastUsedAt != nil {
		m["last_used_at"] = *p.LastUsedAt
	}
	return m
}

func (f *Fake) passkeyRoute(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChallengeID string          `json:"challenge_id"`
		Credential  json.RawMessage `json:"credential"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.passkeysEnabled {
		writeErr(w, 404, "passkey_disabled", "Passkeys are disabled")
		return
	}
	var cred struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body.Credential, &cred)
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && p == "/passkeys/authentication/options":
		id, ch := f.newChallengeLocked(false, "")
		writeJSON(w, 200, map[string]any{"challenge_id": id, "expires_at": f.nowLocked().Add(5 * time.Minute).Unix(),
			"options": map[string]any{"challenge": ch, "timeout": 300000, "rpId": "localhost", "userVerification": "preferred"}})
	case r.Method == http.MethodPost && p == "/passkeys/authentication/verify":
		f.lastCredential = append([]byte(nil), body.Credential...)
		if !f.takeChallengeLocked(body.ChallengeID, false, "") {
			writeErr(w, 400, "webauthn_challenge_not_found", "Challenge not found or already used")
			return
		}
		var pk *FakePasskey
		for _, x := range f.passkeys {
			if x.CredentialID == cred.ID && cred.ID != "" {
				pk = x
			}
		}
		if pk == nil {
			writeErr(w, 400, "webauthn_verification_failed", "Credential verification failed")
			return
		}
		u := f.users[pk.User]
		if u == nil {
			writeErr(w, 400, "webauthn_verification_failed", "Credential verification failed")
			return
		}
		if u.Banned {
			writeErr(w, 403, "user_banned", "User is banned")
			return
		}
		now := f.nowLocked()
		pk.LastUsedAt = &now
		sid := newUUID()
		f.sessions[sid] = &session{user: u.ID, amr: f.amrLocked("passkey")}
		at, rt := f.issueLocked(sid, "")
		f.writeSessionLocked(w, at, rt, u)
	case r.Method == http.MethodPost && p == "/passkeys/registration/options":
		u, ok := f.userFromBearerLocked(w, r)
		if !ok {
			return
		}
		id, ch := f.newChallengeLocked(true, u.ID)
		writeJSON(w, 200, map[string]any{"challenge_id": id, "expires_at": f.nowLocked().Add(5 * time.Minute).Unix(),
			"options": map[string]any{"challenge": ch, "timeout": 300000,
				"rp":               map[string]any{"id": "localhost", "name": "muster"},
				"user":             map[string]any{"id": u.ID, "name": u.Email, "displayName": u.Email},
				"pubKeyCredParams": []any{map[string]any{"type": "public-key", "alg": -7}},
				"authenticatorSelection": map[string]any{"residentKey": "required", "requireResidentKey": true,
					"userVerification": "preferred"},
				"attestation": "none"}})
	case r.Method == http.MethodPost && p == "/passkeys/registration/verify":
		u, ok := f.userFromBearerLocked(w, r)
		if !ok {
			return
		}
		f.lastCredential = append([]byte(nil), body.Credential...)
		if !f.takeChallengeLocked(body.ChallengeID, true, u.ID) {
			writeErr(w, 400, "webauthn_challenge_not_found", "Challenge not found or already used")
			return
		}
		if cred.ID == "" {
			writeErr(w, 400, "webauthn_verification_failed", "Credential verification failed")
			return
		}
		for _, x := range f.passkeys {
			if x.CredentialID == cred.ID {
				writeErr(w, 422, "webauthn_credential_exists", "This credential is already registered")
				return
			}
		}
		id := newUUID()
		pk := &FakePasskey{ID: id, CredentialID: cred.ID, User: u.ID, CreatedAt: f.nowLocked()}
		f.passkeys[id] = pk
		writeJSON(w, 200, passkeyItem(pk))
	case r.Method == http.MethodGet && (p == "/passkeys" || p == "/passkeys/"):
		u, ok := f.userFromBearerLocked(w, r)
		if !ok {
			return
		}
		items := []any{}
		for _, x := range f.passkeys {
			if x.User == u.ID {
				items = append(items, passkeyItem(x))
			}
		}
		writeJSON(w, 200, items)
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/passkeys/"):
		u, ok := f.userFromBearerLocked(w, r)
		if !ok {
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/passkeys/"), "/")
		x := f.passkeys[id]
		if x == nil || x.User != u.ID {
			writeErr(w, 404, "validation_failed", "Passkey not found")
			return
		}
		delete(f.passkeys, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeErr(w, 404, "not_found", "no such route")
	}
}

// adminPasskeyRoute is GET /admin/users/{id}/passkeys and
// DELETE /admin/users/{id}/passkeys/{passkey_id}.
func (f *Fake) adminPasskeyRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/users/")
	uid, tail, _ := strings.Cut(rest, "/passkeys")
	// No passkeysEnabled check: in v2.197.0's router (internal/api/api.go) the
	// admin passkey routes sit under /admin, outside requirePasskeyEnabled.
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.adminPasskeyFailures > 0 {
		f.adminPasskeyFailures--
		writeErr(w, 500, "unexpected_failure", "injected failure")
		return
	}
	if f.users[uid] == nil {
		writeErr(w, 404, "user_not_found", "User not found")
		return
	}
	tail = strings.Trim(tail, "/")
	switch {
	case r.Method == http.MethodGet && tail == "":
		items := []any{}
		for _, x := range f.passkeys {
			if x.User == uid {
				items = append(items, passkeyItem(x))
			}
		}
		writeJSON(w, 200, items)
	case r.Method == http.MethodDelete && tail != "":
		x := f.passkeys[tail]
		if x == nil || x.User != uid {
			writeErr(w, 404, "validation_failed", "Passkey not found")
			return
		}
		delete(f.passkeys, tail)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeErr(w, 404, "not_found", "no such admin route")
	}
}

// --- credentials a browser would send, for tests ------------------------------

// Authenticator-data flag combinations tests use.
const (
	FlagUP       byte = 0x01 // user present only — NO user verification
	FlagUPUV     byte = 0x05 // present and verified
	FlagUPUVBEBS byte = 0x1d // present, verified, backup-eligible, backed up (synced)
)

// AuthData is authenticator data with the given flags: a 32-byte rpIdHash
// (sha256 of "localhost"), the flags byte and a zero sign count.
func AuthData(flags byte) []byte {
	h := sha256.Sum256([]byte("localhost"))
	return append(append(h[:], flags), 0, 0, 0, 0)
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Assertion is a sign-in credential (PublicKeyCredential JSON) for credID
// whose authenticator data carries flags. The fake does not check its
// signature (see the package note above).
func Assertion(credID string, flags byte) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"id": credID, "rawId": b64u([]byte(credID)), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64u([]byte(`{"type":"webauthn.get"}`)),
			"authenticatorData": b64u(AuthData(flags)),
			"signature":         b64u([]byte("signature")),
			"userHandle":        b64u([]byte("user-handle")),
		},
		"clientExtensionResults": map[string]any{},
	})
	return b
}

// Attestation is a registration credential for credID whose attestation
// object ("none" format) carries authenticator data with flags.
func Attestation(credID string, flags byte) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"id": credID, "rawId": b64u([]byte(credID)), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64u([]byte(`{"type":"webauthn.create"}`)),
			"attestationObject": b64u(AttestationObject(AuthData(flags))),
		},
		"clientExtensionResults": map[string]any{},
	})
	return b
}

// AttestationObject CBOR-encodes {"fmt":"none","attStmt":{},"authData":authData}.
func AttestationObject(authData []byte) []byte {
	var out []byte
	out = append(out, CBORHead(5, 3)...)
	out = append(out, CBORText("fmt")...)
	out = append(out, CBORText("none")...)
	out = append(out, CBORText("attStmt")...)
	out = append(out, CBORHead(5, 0)...)
	out = append(out, CBORText("authData")...)
	out = append(out, CBORHead(2, uint64(len(authData)))...)
	return append(out, authData...)
}

// CBORHead encodes a definite-length CBOR item head.
func CBORHead(major byte, n uint64) []byte {
	m := major << 5
	switch {
	case n < 24:
		return []byte{m | byte(n)}
	case n < 1<<8:
		return []byte{m | 24, byte(n)}
	case n < 1<<16:
		return []byte{m | 25, byte(n >> 8), byte(n)}
	default:
		return []byte{m | 26, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	}
}

// CBORText encodes a CBOR text string.
func CBORText(s string) []byte { return append(CBORHead(3, uint64(len(s))), s...) }
