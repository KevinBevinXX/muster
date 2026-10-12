-- muster 0010 — users.sessions_not_before: a per-user session cutoff
-- (accounts PR 5).
--
-- 🔴 A GoTrue BAN DOES NOT END A SESSION; UNBANNING RESURRECTS IT. Measured on
-- supabase/gotrue v2.189.0: a refresh token issued before a ban is refused
-- while the ban stands and ACCEPTED again once it is lifted. So "disable, then
-- enable" would hand every session the user had before the disable back to
-- whoever holds its cookies. Disabling sets this cutoff to the moment of the
-- disable; the session tier refuses any access token issued at or before it —
-- and refuses to REFRESH from one, which is how an old refresh token is
-- reached. Enabling never clears it, so only a sign-in after the enable
-- works.
--
-- NULL means no cutoff. Nothing else reads or writes it.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.users ADD COLUMN IF NOT EXISTS sessions_not_before TIMESTAMPTZ;
