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
    {"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"],
     "Resource": ["arn:aws:s3:::<bucket>/uploads/*"]},
    {"Effect": "Allow", "Action": ["s3:ListBucket", "s3:GetBucketLocation", "s3:GetLifecycleConfiguration"],
     "Resource": ["arn:aws:s3:::<bucket>"]}
  ]
}
```

An expired link stops working, but its object stays in the bucket. Give the
bucket a lifecycle rule that expires `uploads/` objects after longer than the
link lifetime, for example `mc ilm rule add --prefix uploads/ --expire-days 8
<alias>/<bucket>` with the default 24-hour links. At boot muster reads the
bucket's lifecycle (that is what `s3:GetLifecycleConfiguration` is for) and
logs a WARNING when no rule expires `uploads/`. Without that permission it logs
that it could not tell.

## Behaviour

- **Gated like dictation.** `POST /uploads` needs a signed-in, same-origin
  request and asks no step-up: an upload types into nothing. The line reaches a
  session only through Send, which is gated as if it had been typed.
- **The type comes from the bytes**, never from the browser or the filename.
  Only PNG, JPEG, GIF, WebP and PDF are served inline. HTML, SVG, XML and
  anything unrecognised is an attachment typed `application/octet-stream`.
  Every response carries `X-Content-Type-Options: nosniff`, a sandboxing CSP and
  `Cache-Control: private, no-store`.
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
  minutes to deliver its body before it is cut off (408).
- **Downloads are not time-limited.** A slow agent gets the whole file: only
  the store's first response is bounded (30 s), never the transfer. `HEAD` on a
  link answers from the object's metadata without fetching it.

## Not done

- The agent chat page's own message form (and the chief panel's) has no
  paperclip; attach from the agent's Terminal view.
- Thumbnails are the original image scaled by the browser, not a resized copy.
