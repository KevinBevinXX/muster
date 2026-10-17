package api

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/uploads"
)

// ---------------------------------------------------------------------------
// FILE ATTACHMENTS (internal/uploads, migration 0024): the composer's attach
// button, the owner's preview, revocation, and the PUBLIC signed link an agent
// downloads the file through.
//
//	POST /uploads              the composer's upload (multipart `file`)
//	GET  /uploads/{id}/file    the OWNER's view of it (the chip's thumbnail)
//	POST /uploads/{id}/revoke  the owner ends its link (and deletes the bytes)
//	GET  /f/{id}?exp=&sig=     the signed link: no session, the HMAC is the key
//
// 🔴 GATED LIKE DICTATION, NOT LIKE A SEND. An upload types nothing into any
// session: it stores a file and hands the PAGE a line of text. That line reaches
// an agent only through a Send, which is gated (a mirrored session's step-up
// included) exactly as if it had been typed. So POST /uploads is
// requireOperatorWrite — a signed-in session and a same-origin request — and
// asks no step-up; asking one would gate the same paste twice.
//
// 🔴 GET /f/{id} IS PUBLIC, AND EVERYTHING IT SERVES IS DECIDED BY THE SERVER.
// It answers 404 — one answer for every refusal, so a caller learns nothing
// about which check failed — unless the signature matches, the expiry is in
// the future, and the row exists and is not revoked. The type is the sniffed
// one, mapped by uploads.ServeAs (HTML, SVG and XML are never inline); the
// response is never cached and carries a sandboxing CSP.
//
// 🔴 FEATURE OFF IS "THE ROUTES DO NOT EXIST": every route answers 404 and no
// composer draws an attach button (uploadPath). Registration stays
// unconditional (see the package doc in server.go).
// ---------------------------------------------------------------------------

const (
	// uploadRatePerUser and uploadRateWindow: how many files one user may
	// upload in a window — a person attaching a handful, not a loop.
	uploadRatePerUser = 30
	uploadRateWindow  = time.Minute
	// uploadInFlight caps uploads held in memory at once, process-wide: each is
	// up to MaxBytes, held whole while it is sniffed, digested and stored.
	uploadInFlight = 4
	// uploadFieldMax caps each non-file form field.
	uploadFieldMax = 1024
)

// The routes' answers: muster's own sentences, shown by the page as they are.
const (
	UploadTooLarge    = "That file is too large to attach."
	UploadEmpty       = "That file is empty, so there was nothing to attach."
	UploadNoFile      = "No file arrived, so nothing was attached."
	UploadRateLimited = "Too many files in a short time. Wait a moment and try again."
	UploadBusy        = "Other files are being stored right now. Try again in a moment."
	UploadStoreFailed = "The file could not be stored, so nothing was attached. Try again in a moment."
)

// uploadLimits is the upload route's throttle state; the zero value is ready.
type uploadLimits struct {
	perUser boundedWindow
	mu      sync.Mutex
	running int
}

func (l *uploadLimits) enter() (leave func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running >= uploadInFlight {
		return nil, false
	}
	l.running++
	return func() {
		l.mu.Lock()
		l.running--
		l.mu.Unlock()
	}, true
}

func (s *Server) registerUploadRoutes(mux Mux) {
	mux.HandleFunc("POST "+ui.UploadPath, s.requireOperatorWrite(s.handleUpload))
	mux.HandleFunc("GET "+ui.UploadPath+"/{id}/file", s.requireSession(s.handleUploadFile))
	mux.HandleFunc("POST "+ui.UploadPath+"/{id}/revoke", s.requireOperatorWrite(s.handleUploadRevoke))
	mux.HandleFunc("GET "+uploads.LinkPath+"{id}", s.handleUploadLink)
}

// uploadPath is what a composer's attach button posts to: ui.UploadPath when
// uploads are configured, "" (no button drawn) when they are not. The ONE
// predicate every view reads, so a page and the route cannot disagree.
func (s *Server) uploadPath() string {
	if s.ext.Uploads == nil {
		return ""
	}
	return ui.UploadPath
}

// uploadReply is what POST /uploads answers: everything the composer's chip
// draws, and the line the message will carry.
type uploadReply struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Filename    string `json:"filename"`
	ContentType string `json:"type"`
	Size        int64  `json:"size"`
	SizeLabel   string `json:"sizeLabel"`
	Image       bool   `json:"image"`
	Preview     string `json:"preview"`
	Revoke      string `json:"revoke"`
	Line        string `json:"line"`
	Expires     string `json:"expires"`
}

// uploadTarget is which composer a file was attached in, from the form's own
// address fields: a mirrored window, or an agent.
func uploadTarget(fields map[string]string) (kind, target string) {
	if fields["host"] != "" {
		return "mirror", capString(fields["host"]+"/"+fields["session"]+"/"+fields["window"], 300)
	}
	if fields["agent"] != "" {
		return "agent", capString(fields["agent"], 300)
	}
	return "", ""
}

func capString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// handleUpload is POST /uploads: one multipart `file`, and optionally the
// composer's address fields (host, session, window, agent).
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	svc := s.ext.Uploads
	if svc == nil {
		http.NotFound(w, r)
		return
	}
	outcome, size, kind := 0, int64(0), ""
	defer func() {
		s.logger.Printf("uploads: upload user=%d status=%d bytes=%d type=%s", requestOwner(r.Context()), outcome, size, kind)
	}()
	say := func(status int, msg string) {
		outcome = status
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, msg)
	}
	tooLarge := func() {
		w.Header().Set("X-Muster-Max-Bytes", strconv.FormatInt(svc.MaxBytes, 10))
		say(http.StatusRequestEntityTooLarge, UploadTooLarge)
	}
	// The body may carry the multipart framing and the address fields beside
	// the file; anything past this is refused before it is read.
	limit := svc.MaxBytes + 64<<10
	if r.ContentLength > limit {
		tooLarge()
		return
	}
	if !s.uploadLimits.perUser.take(strconv.FormatInt(requestOwner(r.Context()), 10), s.now(), uploadRateWindow, uploadRatePerUser) {
		w.Header().Set("Retry-After", strconv.Itoa(int(uploadRateWindow/time.Second)))
		say(http.StatusTooManyRequests, UploadRateLimited)
		return
	}
	leave, ok := s.uploadLimits.enter()
	if !ok {
		w.Header().Set("Retry-After", "5")
		say(http.StatusServiceUnavailable, UploadBusy)
		return
	}
	defer leave()

	r.Body = http.MaxBytesReader(w, r.Body, limit)
	mr, err := r.MultipartReader()
	if err != nil {
		say(http.StatusBadRequest, UploadNoFile)
		return
	}
	fields := map[string]string{}
	var data []byte
	var filename string
	seenFile := false
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				tooLarge()
				return
			}
			say(http.StatusBadRequest, UploadNoFile)
			return
		}
		name := part.FormName()
		if name == "file" && !seenFile {
			seenFile = true
			filename = part.FileName()
			var buf bytes.Buffer
			n, err := io.Copy(&buf, io.LimitReader(part, svc.MaxBytes+1))
			size = n
			if err != nil {
				var tooBig *http.MaxBytesError
				if errors.As(err, &tooBig) {
					tooLarge()
					return
				}
				say(http.StatusBadRequest, UploadNoFile)
				return
			}
			if n > svc.MaxBytes {
				tooLarge()
				return
			}
			data = buf.Bytes()
			continue
		}
		switch name {
		case "host", "session", "window", "agent":
			b, _ := io.ReadAll(io.LimitReader(part, uploadFieldMax))
			fields[name] = strings.TrimSpace(string(b))
		}
	}
	if !seenFile {
		say(http.StatusBadRequest, UploadNoFile)
		return
	}
	if len(data) == 0 {
		say(http.StatusBadRequest, UploadEmpty)
		return
	}
	tk, target := uploadTarget(fields)
	st, err := svc.Put(r.Context(), data, filename, tk, target)
	if err != nil {
		switch {
		case errors.Is(err, uploads.ErrTooLarge):
			tooLarge()
		case errors.Is(err, uploads.ErrEmpty):
			say(http.StatusBadRequest, UploadEmpty)
		default:
			s.logger.Printf("uploads: storing a file failed: %v", err)
			say(http.StatusBadGateway, UploadStoreFailed)
		}
		return
	}
	outcome, kind = http.StatusCreated, st.ContentType
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusCreated, uploadReply{
		ID: st.PublicID, Name: st.Name, Filename: st.Filename, ContentType: st.ContentType, Size: st.Size,
		SizeLabel: uploads.HumanSize(st.Size), Image: uploads.InlineImage(st.ContentType),
		Preview: ui.UploadFilePath(st.PublicID), Revoke: ui.UploadRevokePath(st.PublicID),
		Line: st.Line, Expires: st.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// serveUpload streams an opened file with the headers uploads.ServeAs decides.
func serveUpload(w http.ResponseWriter, f uploads.Opened) {
	defer f.Body.Close()
	sv := uploads.ServeAs(f.ContentType)
	h := w.Header()
	h.Set("Content-Type", sv.ContentType)
	h.Set("Content-Disposition", uploads.Disposition(sv, f.Filename))
	h.Set("X-Content-Type-Options", "nosniff")
	// Revocable and short-lived: no cache — Cloudflare's included — may keep it.
	h.Set("Cache-Control", "private, no-store")
	// A document served from muster's origin runs nothing and loads nothing.
	// A PDF is not sandboxed (Chromium refuses to show a PDF in a sandboxed
	// document); its viewer runs in the browser's own origin, not muster's.
	if sv.ContentType == "application/pdf" {
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	} else {
		h.Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'; sandbox")
	}
	if f.Size >= 0 {
		h.Set("Content-Length", strconv.FormatInt(f.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f.Body)
}

// handleUploadLink is GET /f/{id}?exp=…&sig=…: the agent's download.
func (s *Server) handleUploadLink(w http.ResponseWriter, r *http.Request) {
	svc := s.ext.Uploads
	if svc == nil {
		http.NotFound(w, r)
		return
	}
	f, err := svc.OpenLink(r.Context(), r.PathValue("id"), r.URL.Query())
	if err != nil {
		if uploads.IsRefusal(err) {
			// One answer for every refusal. The reason is logged, never the
			// signature.
			s.logger.Printf("uploads: link refused for %s: %v", capString(r.PathValue("id"), 32), err)
			w.Header().Set("Cache-Control", "no-store")
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("uploads: link for %s could not be served: %v", r.PathValue("id"), err)
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "the file could not be read right now", http.StatusBadGateway)
		return
	}
	serveUpload(w, f)
}

// handleUploadFile is GET /uploads/{id}/file: the owner's own copy (the
// chip's thumbnail and its Open link). Row-level security decides whose.
func (s *Server) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	svc := s.ext.Uploads
	if svc == nil {
		http.NotFound(w, r)
		return
	}
	f, err := svc.OpenOwned(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, uploads.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("uploads: owner view of %s: %v", r.PathValue("id"), err)
		http.Error(w, "the file could not be read right now", http.StatusBadGateway)
		return
	}
	serveUpload(w, f)
}

// handleUploadRevoke is POST /uploads/{id}/revoke: the link stops working at
// once (204). A page's own fetch, or a plain form (redirected back).
func (s *Server) handleUploadRevoke(w http.ResponseWriter, r *http.Request) {
	svc := s.ext.Uploads
	if svc == nil {
		http.NotFound(w, r)
		return
	}
	objErr, err := svc.Revoke(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, uploads.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("uploads: revoking %s: %v", r.PathValue("id"), err)
		http.Error(w, "the link could not be revoked", http.StatusInternalServerError)
		return
	}
	if objErr != nil {
		// Revoked: the link answers 404 from now on. Only the bytes linger.
		s.logger.Printf("uploads: %s revoked; %v", r.PathValue("id"), objErr)
	}
	s.logger.Printf("uploads: user=%d revoked %s", requestOwner(r.Context()), r.PathValue("id"))
	if isDocumentNavigation(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
