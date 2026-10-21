# File attachments

The composer on a mirrored window's Rendered and Terminal views, and on a
dispatched agent's Terminal view, can attach files: tap the paperclip, paste an
image, or drop a file on the composer. The file is uploaded at once and shows
above the input as a chip (a thumbnail for an image, an icon for anything else,
its name and size). × takes it off again and ends its link.

On Send, each attachment adds one line to the message:

```
[attached: shot.png (image/png, 312 KB) https://muster.example/f/<id>?exp=…&sig=… — download with curl -sSfo shot.png 'https://muster.example/f/<id>?exp=…&sig=…']
```

The agent gets a **short-lived link on muster's own public URL**, signed by
muster. muster streams the file from the object store when the link is
fetched, so the link works from any host that can reach muster, whether or not
it can reach the object store. In the transcript, the line is drawn as the same
chip, never as the raw link, with **Revoke** beside it.

## Configuration

See [configuration](configuration.md#muster-server-optional-features) for the
variables: `MUSTER_S3_ENDPOINT`, `MUSTER_S3_BUCKET`, `MUSTER_S3_ACCESS_KEY`,
`MUSTER_S3_SECRET_KEY`, `MUSTER_S3_REGION`, `MUSTER_UPLOAD_SIGNING_KEY`,
`MUSTER_UPLOAD_MAX_BYTES` and `MUSTER_UPLOAD_LINK_TTL`. `MUSTER_PUBLIC_URL`
and a database are also needed.

A partial or invalid set turns the feature **off**, not the server: no
paperclip is drawn, every upload route and `GET /f/{id}` answer 404, and the
boot banner's `file attachments:` line says `OFF (MISCONFIGURED)` and why.

The access key needs only this, on the bucket (S3/MinIO policy):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect": "Allow",
     "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"],
     "Resource": ["arn:aws:s3:::<bucket>/uploads/*"]},
    {"Effect": "Allow",
     "Action": ["s3:ListBucket", "s3:GetBucketLocation", "s3:GetLifecycleConfiguration",
                "s3:ListBucketMultipartUploads"],
     "Resource": ["arn:aws:s3:::<bucket>"]}
  ]
}
```

`s3:AbortMultipartUpload` and `s3:ListBucketMultipartUploads` are what let a
failed upload be cleaned up. A file over 5 MiB is written as a multipart
upload, and when the upload fails (the client goes away, the read deadline
passes, the file is over the cap) muster aborts it and removes anything it
left. Without these two permissions the parts stay in the bucket.
(`s3:ListMultipartUploadParts` is not needed: measured against MinIO, cleanup
works without it.)

Give the bucket a lifecycle rule for `uploads/` that does both of these:

- **expires objects** after longer than the link lifetime. An expired link
  stops working, but its object stays in the bucket.
- **aborts incomplete multipart uploads**, for a muster process killed in the
  middle of an upload, which cannot clean up after itself.

```json
{"Rules": [{"ID": "muster-uploads", "Status": "Enabled", "Filter": {"Prefix": "uploads/"},
            "Expiration": {"Days": 8},
            "AbortIncompleteMultipartUpload": {"DaysAfterInitiation": 1}}]}
```

Apply it with `mc ilm import <alias>/<bucket> < rule.json`, or your store's
equivalent. ⚠ **MinIO accepts the rule but drops its
`AbortIncompleteMultipartUpload` action** (measured: `mc ilm export` returns
the rule without it). On MinIO, abandoned uploads are removed by the server's
own `api stale_uploads_expiry` (24h by default) instead.

At boot muster reads the bucket's lifecycle (that is what
`s3:GetLifecycleConfiguration` is for). It logs a WARNING for each action that
no rule covers on `uploads/`, so on MinIO the incomplete-upload warning always
appears and names `stale_uploads_expiry`. Without the permission, it logs that
it could not tell.

## Behaviour

- **Gated like dictation.** `POST /uploads` needs a signed-in, same-origin
  request and asks no step-up: an upload types into nothing. The line reaches a
  session only through Send, which is gated as if it had been typed.
- **The type comes from the bytes**, never from the browser or the filename.
  Only PNG, JPEG, GIF and WebP are served inline. A PDF is an attachment typed
  `application/pdf`; HTML, SVG, XML and anything unrecognised is an attachment
  typed `application/octet-stream`. See [Serving uploaded files](#serving-uploaded-files)
  for the headers every response carries.
- **The object key is random** (`uploads/<128-bit id>`). The original filename
  is stored for the owner only; agents and download headers see a reduced name
  of `[A-Za-z0-9._-]` characters.
- **`GET /f/{id}` answers 404** for a bad or tampered signature, an expired
  link, a revoked upload or an unknown id: one answer for all of them. The
  signature is HMAC-SHA256 over the id and the expiry, compared in constant
  time.
- **Revoke** (in the transcript, or × in the composer) marks the upload
  revoked, so its link answers 404 at once, and deletes the object.
- **Thumbnails** are served through `GET /uploads/{id}/file`, which only the
  owner's session can read, never through the signed link.
- **Limits:** 25 MiB per file by default (up to 100 MiB). A file is streamed to
  the store in 5 MiB parts, never held whole. Each user can have 2 uploads in
  progress at once, under a server-wide ceiling of 8, and a request has 3
  minutes to deliver its body before it is cut off (408). A body that ends
  before its boundary is refused (400), never stored truncated.
- **Downloads are not time-limited.** A slow agent gets the whole file: only
  the store's first response is bounded (30 s), never the transfer. `HEAD` on a
  link answers from the object's metadata without fetching it.

## Serving uploaded files

A file someone uploaded is served from muster's own origin, so a file that a
browser renders as a page (HTML, SVG, XHTML, or anything it sniffs as one)
would run its script **as muster**, with the signed-in operator's session:
stored XSS. Three routes serve uploaded bytes, and all three answer through one
function, `uploads.SetServeHeaders`:

| Route | Who | Bytes |
|---|---|---|
| `GET /f/{id}?exp=…&sig=…` | anyone with the signed link (an agent) | an upload, from the object store |
| `GET /uploads/{id}/file` | the owner's session (thumbnails, Open) | an upload, from the object store |
| `GET /tasks/{id}/attachments/{aid}` | a session | a task's attachment, from Postgres |

Each response is decided from the type **sniffed from the bytes** at serve
time, never from what the uploader claimed (a task attachment's stored type is
not read: rows written before this rule hold the uploader's claim). Every
response carries all of:

- `Content-Disposition: attachment`, except for PNG, JPEG, GIF and WebP, which
  are `inline` so a thumbnail can show. SVG and PDF are attachments. The
  filename is reduced to `[A-Za-z0-9._-]`, so it cannot break out of its quotes.
- a `Content-Type` that never names markup: HTML, SVG and XML go out as
  `application/octet-stream`.
- `X-Content-Type-Options: nosniff`, so the browser keeps the type it was given.
- `Content-Security-Policy: default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'; sandbox`.
  `sandbox` with no `allow-*` token gives the response an opaque origin and runs
  no script, so even a file that some client did render as a page could not act
  as muster. It replaces the app's own page CSP, which allows inline script.
- `Cache-Control: private, no-store`.

A task attachment's stored `contentType` is the uploader's **label**, kept as
given so the UI can show it; it never decides how the file is served.

The `filename=` parameter is an ASCII fallback (`[A-Za-z0-9._-]`; a stem with
nothing ASCII left keeps its extension, so `報告.pdf` is `file.pdf`). When that
lost anything, an RFC 5987 `filename*=UTF-8''…` carries the original name,
percent-encoded byte by byte, so no quote, `;`, backslash or line break in a
name can reach the header.

muster never hands out a presigned object-store URL: every download is
streamed through muster, so these headers cannot be skipped by fetching the
object directly.

**A separate origin is not needed for this.** The headers above close the hole
for every route (a test opens an uploaded HTML file in a real browser and
watches it not run). A separate download origin (a different registrable
domain, so a script that did run would hold no muster cookie) would be a
further layer; it is recommended if muster ever needs to show an uploaded
document inline, which today it does not.

## Not done

- The agent chat page's own message form (and the chief panel's) has no
  paperclip; attach from the agent's Terminal view.
- Thumbnails are the original image scaled by the browser, not a resized copy.
