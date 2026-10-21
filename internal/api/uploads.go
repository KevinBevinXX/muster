package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
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
// 🔴 AN UPLOAD IS STREAMED, AND BOUNDED THREE WAYS: the file cap (the service
// refuses one byte over MaxBytes, aborting the write), a READ DEADLINE on the
// request body (a client that trickles is cut off, and its slot freed), and an
// in-flight cap PER USER under a small process-wide ceiling — so one member's
// slow uploads cannot hold the slots everyone else needs.
//
// 🔴 GET /f/{id} IS PUBLIC, AND EVERYTHING IT SERVES IS DECIDED BY THE SERVER.
// It answers 404 — one answer for every refusal, so a caller learns nothing
// about which check failed — unless the signature matches, the expiry is in
// the future, and the row exists and is not revoked. The type is the sniffed
// one, mapped by uploads.ServeAs (HTML, SVG and XML are never inline); the
// response is never cached and carries a sandboxing CSP. HEAD is answered
// from the object's metadata, without fetching its body.
//
// 🔴 FEATURE OFF IS "THE ROUTES DO NOT EXIST": every route answers 404 and no
// composer draws an attach button (uploadPath). Registration stays
// unconditional (see the package doc in server.go).
// ---------------------------------------------------------------------------

const (
	// uploadInFlightPerUser caps one user's uploads in progress at once.
	uploadInFlightPerUser = 2
	// uploadInFlightTotal is the process-wide ceiling. Each upload holds about
	// two uploads.PartSize buffers, so this bounds upload memory at ~80 MiB.
	uploadInFlightTotal = 8
	// defaultUploadReadTimeout is how long a request has to deliver its whole
	// body: 25 MiB at a slow phone's ~200 KB/s, with room.
	defaultUploadReadTimeout = 3 * time.Minute
	// refusalLogEvery is the most often a refused link is logged; the rest
	// are counted and reported with the next line.
	refusalLogEvery = time.Second
)

// The routes' answers: muster's own sentences, shown by the page as they are.
const (
	UploadTooLarge    = "That file is too large to attach."
	UploadEmpty       = "That file is empty, so there was nothing to attach."
	UploadNoFile      = "No file arrived, so nothing was attached."
	UploadBusy        = "Other files of yours are still uploading. Try again when they finish."
	UploadServerBusy  = "Too many files are being stored right now. Try again in a moment."
	UploadTooSlow     = "The file did not arrive in time, so nothing was attached. Try again."
	UploadCutOff      = "The file did not arrive whole, so nothing was attached. Try again."
	UploadStoreFailed = "The file could not be stored, so nothing was attached. Try again in a moment."
)

// uploadLimits is the upload route's in-flight state; the zero value is ready.
type uploadLimits struct {
	mu     sync.Mutex
	total  int
	byUser map[int64]int
	// readTimeout overrides defaultUploadReadTimeout (a test sets it short).
	readTimeout time.Duration
	// refusal-log throttle for GET /f/{id}.
	lastRefusalLog time.Time
	suppressed     int
}

// errUserBusy and errServerBusy are enter's two refusals.
var (
	errUserBusy   = errors.New("this user's uploads are all in flight")
	errServerBusy = errors.New("the process-wide upload ceiling is reached")
)

// enter claims an in-flight slot for owner; the caller calls leave when done.
func (l *uploadLimits) enter(owner int64) (leave func(), err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byUser == nil {
		l.byUser = map[int64]int{}
	}
	if l.byUser[owner] >= uploadInFlightPerUser {
		return nil, errUserBusy
	}
	if l.total >= uploadInFlightTotal {
		return nil, errServerBusy
	}
	l.byUser[owner]++
	l.total++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.total--
		if l.byUser[owner]--; l.byUser[owner] <= 0 {
			delete(l.byUser, owner)
		}
	}, nil
}

func (l *uploadLimits) timeout() time.Duration {
	if l.readTimeout > 0 {
		return l.readTimeout
	}
	return defaultUploadReadTimeout
}

// logRefusal reports whether a refused link may be logged now, and how many
// were suppressed since the last one that was.
func (l *uploadLimits) logRefusal(now time.Time) (ok bool, suppressed int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastRefusalLog) < refusalLogEvery {
		l.suppressed++
		return false, 0
	}
	l.lastRefusalLog, suppressed, l.suppressed = now, l.suppressed, 0
	return true, suppressed
}

// loggedID is an id as it may appear in the log: a well-formed public id, or
// a placeholder. 🔴 The path value is attacker-chosen on the public route and
// decoded (a %0A is a newline), so it is never logged as it came.
func loggedID(id string) string {
	if uploads.ValidPublicID(id) {
		return id
	}
	return "(malformed id)"
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

// handleUpload is POST /uploads: one multipart `file` part, streamed to the
// store.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	svc := s.ext.Uploads
	if svc == nil {
		http.NotFound(w, r)
		return
	}
	owner := requestOwner(r.Context())
	outcome, kind := 0, ""
	var size int64
	defer func() {
		s.logger.Printf("uploads: upload user=%d status=%d bytes=%d type=%s", owner, outcome, size, kind)
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
	// The body may carry the multipart framing beside the file; anything past
	// this is refused before it is read.
	limit := svc.MaxBytes + 64<<10
	if r.ContentLength > limit {
		tooLarge()
		return
	}
	leave, err := s.uploadLimits.enter(owner)
	if err != nil {
		w.Header().Set("Retry-After", "5")
		if errors.Is(err, errUserBusy) {
			say(http.StatusTooManyRequests, UploadBusy)
		} else {
			say(http.StatusServiceUnavailable, UploadServerBusy)
		}
		return
	}
	defer leave()
	// The whole body must arrive within the read timeout: a trickling client
	// is cut off rather than holding its slot. The store is given the same
	// bound.
	deadline := time.Now().Add(s.uploadLimits.timeout())
	_ = http.NewResponseController(w).SetReadDeadline(deadline)
	ctx, cancel := context.WithDeadline(r.Context(), deadline.Add(30*time.Second))
	defer cancel()

	r.Body = http.MaxBytesReader(w, r.Body, limit)
	mr, err := r.MultipartReader()
	if err != nil {
		say(http.StatusBadRequest, UploadNoFile)
		return
	}
	for {
		part, err := mr.NextPart()
		if err != nil {
			var tooBig *http.MaxBytesError
			switch {
			case errors.As(err, &tooBig):
				tooLarge()
			case isTimeout(err):
				say(http.StatusRequestTimeout, UploadTooSlow)
			default:
				say(http.StatusBadRequest, UploadNoFile)
			}
			return
		}
		if part.FormName() != "file" {
			continue
		}
		st, err := svc.Put(ctx, part, part.FileName())
		if err != nil {
			var tooBig *http.MaxBytesError
			switch {
			case errors.Is(err, uploads.ErrTooLarge), errors.As(err, &tooBig):
				tooLarge()
			case errors.Is(err, uploads.ErrEmpty):
				say(http.StatusBadRequest, UploadEmpty)
			case errors.Is(err, uploads.ErrTruncated):
				say(http.StatusBadRequest, UploadCutOff)
			case isTimeout(err):
				say(http.StatusRequestTimeout, UploadTooSlow)
			default:
				s.logger.Printf("uploads: storing a file failed: %v", err)
				say(http.StatusBadGateway, UploadStoreFailed)
			}
			return
		}
		outcome, kind, size = http.StatusCreated, st.ContentType, st.Size
		w.Header().Set("Cache-Control", "no-store")
		s.writeJSON(w, http.StatusCreated, uploadReply{
			ID: st.PublicID, Name: st.Name, Filename: st.Filename, ContentType: st.ContentType, Size: st.Size,
			SizeLabel: uploads.HumanSize(st.Size), Image: uploads.InlineImage(st.ContentType),
			Preview: ui.UploadFilePath(st.PublicID), Revoke: ui.UploadRevokePath(st.PublicID),
			Line: st.Line, Expires: st.ExpiresAt.UTC().Format(time.RFC3339),
		})
		return
	}
}

// isTimeout reports whether err is a deadline: the body's read deadline, or
// the context's.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var te interface{ Timeout() bool }
	return errors.As(err, &te) && te.Timeout()
}

// writeUploadHeaders sets the headers uploads.SetServeHeaders decides.
func writeUploadHeaders(w http.ResponseWriter, f uploads.Opened) {
	uploads.SetServeHeaders(w.Header(), f.ContentType, f.Filename, f.Size)
}

// serveUpload streams an opened file with its headers.
func serveUpload(w http.ResponseWriter, f uploads.Opened) {
	defer f.Body.Close()
	writeUploadHeaders(w, f)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f.Body)
}

// handleUploadLink is GET (and HEAD) /f/{id}?exp=…&sig=…: the agent's
// download.
func (s *Server) handleUploadLink(w http.ResponseWriter, r *http.Request) {
	svc := s.ext.Uploads
	if svc == nil {
		http.NotFound(w, r)
		return
	}
	id := r.PathValue("id")
	var f uploads.Opened
	var err error
	if r.Method == http.MethodHead {
		f, err = svc.StatLink(r.Context(), id, r.URL.Query())
	} else {
		f, err = svc.OpenLink(r.Context(), id, r.URL.Query())
	}
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		if uploads.IsRefusal(err) {
			// One answer for every refusal. The reason is logged — never the
			// signature, never an unvalidated id, and at most once a second.
			if ok, n := s.uploadLimits.logRefusal(s.now()); ok {
				s.logger.Printf("uploads: link refused for %s: %v (%d more refused since the last line)", loggedID(id), err, n)
			}
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("uploads: link for %s could not be served: %v", loggedID(id), err)
		http.Error(w, "the file could not be read right now", http.StatusBadGateway)
		return
	}
	if f.Body == nil {
		writeUploadHeaders(w, f)
		w.WriteHeader(http.StatusOK)
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
		s.logger.Printf("uploads: owner view of %s: %v", loggedID(r.PathValue("id")), err)
		http.Error(w, "the file could not be read right now", http.StatusBadGateway)
		return
	}
	serveUpload(w, f)
}

// handleUploadRevoke is POST /uploads/{id}/revoke: the link stops working at
// once (204). A page's own fetch, or a plain form (redirected home).
func (s *Server) handleUploadRevoke(w http.ResponseWriter, r *http.Request) {
	svc := s.ext.Uploads
	if svc == nil {
		http.NotFound(w, r)
		return
	}
	id := r.PathValue("id")
	objErr, err := svc.Revoke(r.Context(), id)
	if err != nil {
		if errors.Is(err, uploads.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("uploads: revoking %s: %v", loggedID(id), err)
		http.Error(w, "the link could not be revoked", http.StatusInternalServerError)
		return
	}
	if objErr != nil {
		// Revoked: the link answers 404 from now on. Only the bytes linger.
		s.logger.Printf("uploads: %s revoked; %v", loggedID(id), objErr)
	}
	s.logger.Printf("uploads: user=%d revoked %s", requestOwner(r.Context()), loggedID(id))
	if isDocumentNavigation(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
