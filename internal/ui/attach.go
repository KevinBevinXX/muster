package ui

import (
	"encoding/json"
	"net/url"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/uploads"
)

// FILE ATTACHMENTS IN THE COMPOSER (internal/uploads; internal/api's
// uploads.go).
//
// A paperclip beside the input (and pasting an image, or dropping a file on
// the composer) uploads the file at once; it then sits ABOVE the input as a
// chip — a thumbnail for an image, an icon for anything else, its name and
// size, and × to take it off again (which revokes its link). On Send, each
// chip's line (uploads.AgentLine: a short description and a muster-signed
// link) is appended to the message, so the agent receives text it can act on
// and the person never sees the raw signed URL.
//
// 🔴 THE LINE IS ADDED WHERE THE TEXT IS SENT, BY THE COMPOSER'S OWN SCRIPT
// (window.musterAttach.compose, called by mirrorComposerScript's body() and by
// native() before the browser submits). Every send route, the queue
// included, receives an ordinary message: none of them needed to change, and
// the server's checks on a message (size, plain text, step-up) apply to the
// line exactly as if it had been typed.
//
// In a TRANSCRIPT the same line is drawn as the same chip (promptBody), its
// thumbnail through the OWNER's route (UploadFilePath, session-gated) — never
// through the signed link — with Revoke beside it.

// UploadPath is where the composer posts a file.
const UploadPath = "/uploads"

// UploadFilePath is the owner's view of upload id (thumbnail, Open).
func UploadFilePath(id string) string { return UploadPath + "/" + url.PathEscape(id) + "/file" }

// UploadRevokePath is where the owner revokes upload id's link.
func UploadRevokePath(id string) string { return UploadPath + "/" + url.PathEscape(id) + "/revoke" }

// The attach control's and chips' accessible names and inline messages.
const (
	attachLabel          = "Attach a file"
	AttachRemoveLabel    = "Remove attachment"
	AttachRevokeLabel    = "Revoke this link"
	AttachRevoked        = "Link revoked"
	AttachUploading      = "Uploading…"
	AttachFailed         = "That file could not be attached."
	AttachStillUploading = "Wait for the attachment to finish uploading."
)

// attachButtonClass is the paperclip's look: the mic's (dictateButtonClass)
// 44px square at rest.
const attachButtonClass = "press inline-flex min-h-[44px] min-w-[44px] shrink-0 items-center justify-center rounded-xl bg-s2 text-fg2 ring-1 ring-inset ring-line transition disabled:opacity-60"

// attachControl is the paperclip and its hidden file input. Nothing when path
// is "" (uploads are not configured).
func attachControl(path string) g.Node {
	if path == "" {
		return nil
	}
	return g.Group([]g.Node{
		// Outside the form's submitted fields: no name, and the script posts it
		// itself (a native submit never carries a file).
		Input(ID("mv-file"), Type("file"), g.Attr("multiple", ""), g.Attr("hidden", ""), g.Attr("tabindex", "-1"),
			g.Attr("aria-hidden", "true")),
		Button(ID("mv-attach"), Type("button"), g.Attr("data-upload", path),
			g.Attr("aria-label", attachLabel), g.Attr("title", attachLabel), Class(attachButtonClass),
			g.Raw(`<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m21.44 11.05-9.19 9.19a6 6 0 0 1-8.49-8.49l8.57-8.57A4 4 0 1 1 18 8.84l-8.59 8.57a2 2 0 0 1-2.83-2.83l8.49-8.48"/></svg>`)),
	})
}

// attachBox is where the composer's chips sit, above the input. Hidden while
// empty. Nothing when path is "".
func attachBox(path string) g.Node {
	if path == "" {
		return nil
	}
	return Div(ID("mv-attachments"), g.Attr("hidden", ""), g.Attr("aria-label", "Attachments"),
		Class("flex flex-wrap gap-2 px-3 pt-2"))
}

// chipClass is a chip's frame, in the composer and in a transcript alike.
const chipClass = "inline-flex max-w-full items-center gap-2 rounded-xl bg-s2 px-2 py-1.5 text-xs text-fg ring-1 ring-inset ring-line"

// fileIcon is the chip's glyph for a file that is not an image.
const fileIcon = `<svg class="h-6 w-6 shrink-0 text-fg2" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/></svg>`

// attachmentChip is one attachment as a transcript shows it.
func attachmentChip(ref uploads.Ref) g.Node {
	preview := UploadFilePath(ref.PublicID)
	var thumb g.Node = g.Raw(fileIcon)
	if ref.IsImage() {
		thumb = Img(Src(preview), Alt(""), g.Attr("loading", "lazy"), g.Attr("data-attachment-thumb", ""),
			Class("h-10 w-10 shrink-0 rounded-lg bg-s3 object-cover"))
	}
	return Span(
		g.Attr("data-attachment-chip", ""), g.Attr("data-upload-id", ref.PublicID),
		Class(chipClass),
		A(Href(preview), Target("_blank"), Rel("noopener"), Class("inline-flex min-w-0 items-center gap-2"),
			g.Attr("title", "Open "+ref.Name),
			thumb,
			Span(Class("flex min-w-0 flex-col text-left"),
				Span(Class("truncate font-medium"), g.Text(ref.Name)),
				Span(g.Attr("data-attachment-meta", ""), Class("truncate text-muted"), g.Text(ref.ContentType+" · "+ref.Size)),
			),
		),
		Button(Type("button"), g.Attr("data-upload-revoke", UploadRevokePath(ref.PublicID)),
			g.Attr("aria-label", AttachRevokeLabel), g.Attr("title", AttachRevokeLabel),
			Class("press shrink-0 rounded-lg px-1.5 py-1 text-muted hover:text-st-error-fg"), g.Text("Revoke")),
	)
}

// promptBody is a message's text as a transcript bubble shows it: markdown,
// with every attachment line (uploads.AgentLine) drawn as its chip in place of
// the raw signed URL.
func promptBody(text string) g.Node {
	refs := uploads.FindRefs(text)
	if len(refs) == 0 {
		return renderMarkdown(text)
	}
	var out []g.Node
	var chips []g.Node
	flush := func() {
		if len(chips) > 0 {
			out = append(out, Div(g.Attr("data-attachments", ""), Class("my-1 flex flex-wrap gap-2"), g.Group(chips)))
			chips = nil
		}
	}
	at := 0
	for _, r := range refs {
		// (Two slices, not one with both bounds: Tailwind's scanner reads that
		// spelling in a .go file as an arbitrary-property class and emits a rule.)
		if seg := text[at:][:r.Start-at]; strings.TrimSpace(seg) != "" {
			flush()
			out = append(out, renderMarkdown(seg))
		}
		chips = append(chips, attachmentChip(r))
		at = r.End
	}
	flush()
	if seg := text[at:]; strings.TrimSpace(seg) != "" {
		out = append(out, renderMarkdown(seg))
	}
	return g.Group(out)
}

// attachScript wires the composer's attachments: the paperclip, paste, drop,
// the chips, and window.musterAttach for mirrorComposerScript. It also wires
// Revoke on every transcript chip of the page. Nothing when path is "".
func attachScript(path string) g.Node {
	if path == "" {
		return nil
	}
	msgs := `{"uploading":` + jsString(AttachUploading) + `,"failed":` + jsString(AttachFailed) +
		`,"remove":` + jsString(AttachRemoveLabel) + `,"revoked":` + jsString(AttachRevoked) +
		`,"pending":` + jsString(AttachStillUploading) + `}`
	return Script(g.Raw(`
(function () {
  var M = ` + msgs + `;
  var form = document.getElementById('mv-form');
  var btn = document.getElementById('mv-attach');
  var picker = document.getElementById('mv-file');
  var box = document.getElementById('mv-attachments');
  var note = document.getElementById('mv-note');
  if (!form || !btn || !picker || !box || form.hasAttribute('data-attach-bound')) return;
  form.setAttribute('data-attach-bound', '');
  var input = document.getElementById('mv-input');
  var path = btn.getAttribute('data-upload');
  var items = [];
  // composed is what the last compose() put in a message.
  var composed = [];
  function say(msg) { if (!note) return; note.textContent = msg || ''; note.hidden = !msg; note.removeAttribute('data-ok'); }
  function sync() { box.hidden = items.length === 0; }
  function text(t, cls) { var s = document.createElement('span'); s.className = cls; s.textContent = t; return s; }
  function chip(item) {
    var el = document.createElement('span');
    el.setAttribute('data-attachment-chip', '');
    el.setAttribute('data-state', 'uploading');
    el.className = '` + chipClass + `';
    var thumb = document.createElement('span');
    thumb.className = 'inline-flex h-10 w-10 shrink-0 items-center justify-center';
    thumb.innerHTML = ` + jsString(fileIcon) + `;
    var col = document.createElement('span');
    col.className = 'flex min-w-0 flex-col text-left';
    var name = text(item.file.name || 'file', 'truncate font-medium max-w-[12rem]');
    var meta = text(M.uploading, 'truncate text-muted');
    meta.setAttribute('data-attachment-meta', '');
    col.appendChild(name); col.appendChild(meta);
    var x = document.createElement('button');
    x.type = 'button';
    x.className = 'press shrink-0 rounded-lg px-1.5 py-1 text-muted hover:text-st-error-fg';
    x.setAttribute('aria-label', M.remove);
    x.setAttribute('title', M.remove);
    x.textContent = '×';
    x.addEventListener('click', function () { remove(item, true); });
    el.appendChild(thumb); el.appendChild(col); el.appendChild(x);
    item.el = el; item.thumb = thumb; item.name = name; item.meta = meta;
    return el;
  }
  function remove(item, revoke) {
    var i = items.indexOf(item);
    if (i >= 0) items.splice(i, 1);
    if (item.el && item.el.parentNode) item.el.parentNode.removeChild(item.el);
    // Taken off before it was sent: its link is ended (the agent never had it).
    if (revoke && item.reply && item.reply.revoke) {
      try { fetch(item.reply.revoke, { method: 'POST', credentials: 'same-origin', headers: { 'X-Requested-With': 'XMLHttpRequest' } }); } catch (e) {}
    }
    sync();
  }
  function upload(file) {
    var item = { file: file, reply: null };
    items.push(item);
    box.appendChild(chip(item));
    sync();
    say('');
    var fd = new FormData();
    fd.append('file', file, file.name || 'pasted');
    fetch(path, { method: 'POST', body: fd, credentials: 'same-origin', headers: { 'X-Requested-With': 'XMLHttpRequest' } })
      .then(function (r) {
        if (!r.ok) return r.text().then(function (t) { throw new Error(t.trim() || M.failed); });
        return r.json();
      })
      .then(function (rep) {
        if (items.indexOf(item) < 0) {
          // Removed while it was uploading: end the link it was just given.
          try { fetch(rep.revoke, { method: 'POST', credentials: 'same-origin', headers: { 'X-Requested-With': 'XMLHttpRequest' } }); } catch (e) {}
          return;
        }
        item.reply = rep;
        item.el.setAttribute('data-state', 'ready');
        item.el.setAttribute('data-upload-id', rep.id);
        item.name.textContent = rep.name;
        item.meta.textContent = rep.type + ' · ' + rep.sizeLabel;
        if (rep.image) {
          var img = document.createElement('img');
          img.src = rep.preview;
          img.alt = '';
          img.setAttribute('data-attachment-thumb', '');
          img.className = 'h-10 w-10 shrink-0 rounded-lg bg-s3 object-cover';
          item.thumb.innerHTML = '';
          item.thumb.appendChild(img);
        }
      }, function (err) {
        remove(item, false);
        say((err && err.message) || M.failed);
      });
  }
  function addAll(files) { for (var i = 0; i < files.length; i++) upload(files[i]); }
  btn.addEventListener('click', function () { picker.click(); });
  picker.addEventListener('change', function () { addAll(picker.files || []); picker.value = ''; });
  if (input) input.addEventListener('paste', function (e) {
    var files = e.clipboardData && e.clipboardData.files;
    if (files && files.length) { e.preventDefault(); addAll(files); }
  });
  var drop = document.getElementById('mv-compose') || form;
  drop.addEventListener('dragover', function (e) { if (e.dataTransfer && Array.prototype.indexOf.call(e.dataTransfer.types || [], 'Files') >= 0) e.preventDefault(); });
  drop.addEventListener('drop', function (e) {
    if (e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files.length) { e.preventDefault(); addAll(e.dataTransfer.files); }
  });
  window.musterAttach = {
    // count is how many attachments are on the composer (uploaded or not).
    count: function () { return items.length; },
    // pending is true while any is still uploading: Send waits for it.
    pending: function () { for (var i = 0; i < items.length; i++) if (!items[i].reply) return true; return false; },
    pendingMessage: M.pending,
    // compose is the message as sent: the text, then one line per attachment.
    compose: function (t) {
      var lines = [];
      composed = [];
      for (var i = 0; i < items.length; i++) if (items[i].reply) { lines.push(items[i].reply.line); composed.push(items[i]); }
      if (!lines.length) return t;
      return (t && t.trim() ? t.replace(/\s+$/, '') + '\n' : '') + lines.join('\n');
    },
    // sent clears the attachments that went with the message — only those: one
    // attached while the send was out stays for the next.
    sent: function () { var done = composed; composed = []; for (var i = 0; i < done.length; i++) remove(done[i], false); }
  };
})();
(function () {
  if (window.__musterAttachRevoke) return;
  window.__musterAttachRevoke = true;
  // Revoke on a transcript's chip: the link stops working at once.
  document.addEventListener('click', function (e) {
    var b = e.target && e.target.closest ? e.target.closest('[data-upload-revoke]') : null;
    if (!b || b.disabled) return;
    e.preventDefault();
    b.disabled = true;
    fetch(b.getAttribute('data-upload-revoke'), { method: 'POST', credentials: 'same-origin', headers: { 'X-Requested-With': 'XMLHttpRequest' } })
      .then(function (r) {
        if (!r.ok) { b.disabled = false; return; }
        var c = b.closest('[data-attachment-chip]');
        if (c) c.setAttribute('data-revoked', '');
        b.textContent = ` + jsString(AttachRevoked) + `;
      }, function () { b.disabled = false; });
  });
})();
`))
}

// jsString is s as a JavaScript string literal, safe inside a <script>
// element: json.Marshal escapes <, > and & (so no "</script>" can close it).
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
