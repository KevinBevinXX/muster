-- muster 0024 — UPLOADS (internal/uploads): a file attached in a message
-- composer. The BYTES are in an S3-compatible object store (e.g. MinIO) at
-- object_key; this row is the metadata, and what a muster-signed link
-- (GET /f/{public_id}?exp=…&sig=…) is checked against: a revoked row's link
-- answers 404 inside its expiry.
--
-- 🔴 THE OBJECT KEY IS RANDOM ('uploads/' || public_id). filename is the
-- browser's name, stored for the owner's eyes only; it never reaches a key.
-- content_type is SNIFFED from the bytes, never the client's.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, IN 0017's SHAPE. A request
-- creates, reads and revokes only its own user's rows. The one read across
-- owners is the signed link's (uploads.PGStore.LinkTarget, under authz.System,
-- after the link's HMAC has been verified).
--
-- 🔴 IT IS RE-APPLIED BY db.RLSOn (rollback.go) WHEN THE TABLE EXISTS, SO EVERY
-- STATEMENT IS IDEMPOTENT.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.uploads (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id     BIGINT      NOT NULL
                 DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                 REFERENCES public.users (id),
    public_id    TEXT        NOT NULL,
    object_key   TEXT        NOT NULL,
    filename     TEXT        NOT NULL,
    content_type TEXT        NOT NULL,
    size_bytes   BIGINT      NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    revoked_at   TIMESTAMPTZ,
    -- 128 random bits, unpadded base64url (uploads.NewPublicID).
    CONSTRAINT uploads_public_id_check CHECK (public_id ~ '^[A-Za-z0-9_-]{22}$'),
    -- The key is the id and nothing else (uploads.ObjectKeyFor).
    CONSTRAINT uploads_object_key_check CHECK (object_key = 'uploads/' || public_id),
    CONSTRAINT uploads_filename_cap CHECK (octet_length(filename) BETWEEN 1 AND 255),
    CONSTRAINT uploads_content_type_cap CHECK (char_length(content_type) BETWEEN 1 AND 100),
    CONSTRAINT uploads_size_check CHECK (size_bytes > 0),
    CONSTRAINT uploads_expiry_check CHECK (expires_at > created_at)
);

CREATE UNIQUE INDEX IF NOT EXISTS uploads_public_id ON public.uploads (public_id);
CREATE INDEX IF NOT EXISTS idx_uploads_owner_created ON public.uploads (owner_id, created_at DESC);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.uploads ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.uploads ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.uploads FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.uploads;
CREATE POLICY owner_isolation ON public.uploads
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

-- No DELETE: a row outlives its object (revocation is an UPDATE), so the
-- owner keeps the record of what was shared and when.
GRANT SELECT, INSERT, UPDATE ON public.uploads TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.uploads_id_seq TO muster_app;
