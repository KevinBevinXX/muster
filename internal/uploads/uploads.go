// Package uploads is the message composer's file attachments (migration 0024):
// a file the operator attaches is stored in an S3-compatible object store
// (e.g. MinIO), and the agent it is sent to is handed a SHORT-LIVED,
// muster-SIGNED link to it on muster's public URL — never the object store's
// own address, which is LAN-only.
//
//	composer ──POST /uploads──▶ muster ──PUT──▶ object store (bucket, random key)
//	agent ──GET <public>/f/<id>?exp=…&sig=…──▶ muster ──GET──▶ object store
//
// 🔴 THE LINK IS MUSTER'S, AND MUSTER STREAMS THE BYTES. The signature is an
// HMAC under a key only this server holds (MUSTER_UPLOAD_SIGNING_KEY), over the
// upload's public id and the link's expiry. GET /f/{id} checks the signature,
// the expiry, and the row — a REVOKED upload answers 404 even inside its
// expiry — before a byte is read from the store. So a link works from any
// host, on or off the LAN, and is both expiring and revocable.
//
// 🔴 NOTHING THE CLIENT SAYS ABOUT THE FILE DECIDES HOW IT IS SERVED. The type
// is SNIFFED from the bytes (Classify); the original filename is stored for the
// owner's eyes and never reaches the object key (NewPublicID is random), and
// what an agent or a Content-Disposition header sees is SafeName's reduction of
// it. Only PNG, JPEG, GIF and WebP are served inline; a PDF is a labelled
// attachment; HTML, SVG, XML, script and anything unrecognised is an
// attachment typed application/octet-stream. SetServeHeaders is the one place
// the response headers are decided.
package uploads

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Upload is one stored file's metadata row.
type Upload struct {
	ID       int64
	OwnerID  int64
	PublicID string
	// ObjectKey is where the bytes are in the bucket: ObjectKeyFor(PublicID).
	ObjectKey string
	// Filename is the name the browser gave, cleaned of control characters and
	// capped (CleanFilename). Shown to the OWNER only; agents see SafeName.
	Filename string
	// ContentType is the type sniffed from the bytes (Classify), never the
	// client's.
	ContentType string
	Size        int64
	CreatedAt   time.Time
	// ExpiresAt is when the link minted at upload stops working.
	ExpiresAt time.Time
	// RevokedAt is set when the owner revoked the link; nil while it works.
	RevokedAt *time.Time
}

// Revoked reports whether the owner revoked the upload's link.
func (u Upload) Revoked() bool { return u.RevokedAt != nil }

// Errors a caller branches on.
var (
	// ErrNotFound is an upload that does not exist or is not the caller's —
	// the two answer identically.
	ErrNotFound = errors.New("uploads: no such upload")
	// ErrTooLarge is a file over the configured cap.
	ErrTooLarge = errors.New("uploads: file is too large")
	// ErrEmpty is a file with no bytes.
	ErrEmpty = errors.New("uploads: file is empty")
)

// publicIDBytes is the entropy of a public id: 128 bits, so an id is not
// guessable — though a link needs its signature as well, so guessing one buys
// nothing on its own.
const publicIDBytes = 16

// publicIDPattern is a public id's spelling: 22 characters of unpadded
// base64url (128 bits).
var publicIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

// ValidPublicID reports whether id is spelled like a public id.
func ValidPublicID(id string) bool { return publicIDPattern.MatchString(id) }

// NewPublicID mints a random public id.
func NewPublicID() (string, error) {
	var b [publicIDBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("uploads: random id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// objectKeyPrefix is every object's prefix in the bucket, so the bucket can be
// shared with nothing else muster writes and a lifecycle rule can name it.
const objectKeyPrefix = "uploads/"

// ObjectKeyFor is the object key of the upload with public id id.
//
// 🔴 THE KEY IS THE RANDOM ID AND NOTHING ELSE. No filename, no owner, no type:
// a name the browser chose never becomes part of a path anywhere.
func ObjectKeyFor(id string) string { return objectKeyPrefix + id }

// maxFilenameBytes caps a stored original filename.
const maxFilenameBytes = 255

// CleanFilename is the browser's filename as stored: its last path element,
// control and format characters removed, invalid UTF-8 dropped, capped at 255
// bytes. "" becomes "file".
func CleanFilename(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = path.Base(name)
	var b strings.Builder
	for _, r := range name {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > maxFilenameBytes {
			break
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if out == "" || out == "." || out == "/" {
		return "file"
	}
	return out
}

// maxSafeName caps SafeName.
const maxSafeName = 80

// SafeName is a filename reduced to what is safe to put anywhere an agent or a
// shell will read it: [A-Za-z0-9._-] only (every other run becomes one "_"),
// never starting with "." or "-" (a hidden file, a flag), capped at 80
// characters with the extension kept. "" becomes "file"; a stem with nothing
// left in it becomes "file" ahead of its extension ("報告.pdf" → "file.pdf").
//
// 🔴 IT IS WHAT MAKES THE curl LINE SAFE TO PASTE. The name is written
// unquoted after `-o` in the line the agent is handed (AgentLine); a name
// that could hold a quote, a space, a `$` or a leading dash would turn a
// download into something else.
func SafeName(name string) string {
	var b strings.Builder
	under := false
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_'
		if ok {
			b.WriteRune(r)
			under = false
			continue
		}
		if !under {
			b.WriteByte('_')
			under = true
		}
	}
	out := strings.TrimLeft(b.String(), ".-_")
	// A stem that was all non-ASCII reduces to nothing, and the trim above
	// would then leave the bare extension ("報告.pdf" → "pdf"). Keep the
	// extension behind a placeholder stem instead. Only when the ORIGINAL had a
	// stem: ".bashrc" is a name with no stem, and stays "bashrc".
	if ext := path.Ext(b.String()); len(ext) > 1 {
		origExt := path.Ext(name)
		origStem := strings.TrimSpace(strings.TrimSuffix(name, origExt))
		if origStem != "" && strings.Trim(strings.TrimSuffix(b.String(), ext), "._-") == "" {
			out = "file" + ext
		}
	}
	if len(out) > maxSafeName {
		ext := path.Ext(out)
		if len(ext) > 16 || len(ext) >= maxSafeName {
			ext = ""
		}
		out = out[:maxSafeName-len(ext)] + ext
	}
	if out == "" || strings.Trim(out, "._-") == "" {
		return "file"
	}
	return out
}

// HumanSize is n bytes as a person reads it: "312 B", "312 KB", "4.2 MB".
// 1 KB is 1024 bytes.
func HumanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%d KB", (n+512)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

// AgentLine is the text an attached file adds to the message the agent
// receives: short, and enough to fetch the file with one command.
//
//	[attached: shot.png (image/png, 312 KB) <url> — download with curl -sSfo shot.png '<url>']
//
// name must already be a SafeName; url is a signed link (Signer.URL), whose
// alphabet holds no quote.
func AgentLine(name, contentType string, size int64, url string) string {
	return "[attached: " + name + " (" + contentType + ", " + HumanSize(size) + ") " + url +
		" — download with curl -sSfo " + name + " '" + url + "']"
}

// agentLinePattern recognises an AgentLine in a message: group 1 the name, 2
// the type, 3 the size, 4 the URL, 5 the public id. It is the ONE parser of the
// format AgentLine writes; the transcript's chips are drawn from what it finds.
var agentLinePattern = regexp.MustCompile(
	`\[attached: ([A-Za-z0-9._-]{1,80}) \(([a-z0-9.+/-]{1,100}(?:; charset=[a-z0-9-]{1,20})?), ([0-9.]{1,8} (?:B|KB|MB|GB))\) ` +
		`(https?://[^\s'\]<>"]{1,300}/f/([A-Za-z0-9_-]{22})\?exp=[0-9]{1,12}&sig=[A-Za-z0-9_-]{43}) ` +
		`— download with curl -sSfo [A-Za-z0-9._-]{1,80} '[^'\s]{1,400}'\]`)

// Ref is one AgentLine found in a message.
type Ref struct {
	// Start and End are its byte offsets in the message.
	Start, End  int
	Name        string
	ContentType string
	Size        string
	URL         string
	PublicID    string
}

// IsImage reports whether the attachment is an image a preview can show.
func (r Ref) IsImage() bool { return InlineImage(r.ContentType) }

// FindRefs is every AgentLine in text, in order.
func FindRefs(text string) []Ref {
	var out []Ref
	for _, m := range agentLinePattern.FindAllStringSubmatchIndex(text, -1) {
		out = append(out, Ref{
			Start: m[0], End: m[1],
			Name:        text[m[2]:m[3]],
			ContentType: text[m[4]:m[5]],
			Size:        text[m[6]:m[7]],
			URL:         text[m[8]:m[9]],
			PublicID:    text[m[10]:m[11]],
		})
	}
	return out
}
