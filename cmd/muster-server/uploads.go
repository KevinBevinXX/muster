package main

import (
	"context"
	"log"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/uploads"
)

// uploadSettings is the uploads configuration, parsed: what buildUploads
// builds a service from, or why it does not.
type uploadSettings struct {
	s3       uploads.S3Config
	signer   uploads.Signer
	maxBytes int64
	ttl      time.Duration
	// off is "" when uploads are on; otherwise the one reason they are not,
	// naming the variable to fix. misconfigured says whether that reason is a
	// MISTAKE (a partial or invalid set) rather than the feature simply unset.
	off           string
	misconfigured bool
}

// uploadEnvNames is every variable the feature reads besides MUSTER_PUBLIC_URL,
// in the order the banner names them.
var uploadEnvNames = []string{envS3Endpoint, envS3Bucket, envS3AccessKey, envS3SecretKey, envS3Region,
	envUploadSigningKey, envUploadMaxBytes, envUploadLinkTTL}

// parseUploads is the deployment policy for file attachments.
//
// 🔴 IT NEVER REFUSES TO BOOT. Unlike dictation's token-without-URL, a broken
// upload configuration turns the FEATURE off and leaves the server serving:
// the attach button is not drawn, every upload route answers 404, and the
// banner says MISCONFIGURED and why. A typo in a bucket name must not take the
// task board down with it.
//
// On requires all five of endpoint, bucket, access key, secret key and signing
// key, plus MUSTER_PUBLIC_URL (the links are written on it) and a database
// (the rows). None of the five set is plain OFF; some of them is MISCONFIGURED.
func parseUploads(cfg config, hasDB bool) uploadSettings {
	required := map[string]string{envS3Endpoint: cfg.S3Endpoint, envS3Bucket: cfg.S3Bucket,
		envS3AccessKey: cfg.S3AccessKey, envS3SecretKey: cfg.S3SecretKey, envUploadSigningKey: cfg.UploadSigningKey}
	var missing []string
	set := 0
	for _, name := range []string{envS3Endpoint, envS3Bucket, envS3AccessKey, envS3SecretKey, envUploadSigningKey} {
		if required[name] == "" {
			missing = append(missing, name)
		} else {
			set++
		}
	}
	bad := func(why string) uploadSettings { return uploadSettings{off: why, misconfigured: true} }
	if set == 0 {
		return uploadSettings{off: "none of the required variables is set"}
	}
	if len(missing) > 0 {
		return bad(strings.Join(missing, ", ") + " unset (all five of " + envS3Endpoint + ", " + envS3Bucket + ", " +
			envS3AccessKey + ", " + envS3SecretKey + " and " + envUploadSigningKey + " are needed)")
	}
	if u, err := url.Parse(cfg.PublicURL); cfg.PublicURL == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return bad(envPublicURL + " is not an http(s) URL, and every agent link is written on it")
	}
	if !hasDB {
		return bad("there is no database (" + envDatabase + "), and every upload is a row")
	}
	s := uploadSettings{maxBytes: uploads.DefaultMaxBytes, ttl: uploads.DefaultLinkTTL}
	if cfg.UploadMaxBytes != "" {
		n, err := strconv.ParseInt(cfg.UploadMaxBytes, 10, 64)
		if err != nil || n < 1 || n > uploads.MaxMaxBytes {
			return bad(envUploadMaxBytes + " must be a byte count from 1 to " + strconv.FormatInt(uploads.MaxMaxBytes, 10))
		}
		s.maxBytes = n
	}
	if cfg.UploadLinkTTL != "" {
		d, err := time.ParseDuration(cfg.UploadLinkTTL)
		if err != nil || d < uploads.MinLinkTTL || d > uploads.MaxLinkTTL {
			return bad(envUploadLinkTTL + " must be a Go duration from " + uploads.MinLinkTTL.String() + " to " + uploads.MaxLinkTTL.String())
		}
		s.ttl = d
	}
	signer, err := uploads.NewSigner(cfg.UploadSigningKey)
	if err != nil {
		return bad(envUploadSigningKey + " is shorter than 32 bytes (mint one with `openssl rand -base64 48`)")
	}
	s.signer = signer
	s.s3 = uploads.S3Config{Endpoint: cfg.S3Endpoint, Bucket: cfg.S3Bucket, AccessKey: cfg.S3AccessKey,
		SecretKey: cfg.S3SecretKey, Region: cfg.S3Region}
	if _, err := uploads.NewS3(s.s3); err != nil {
		return bad(strings.TrimPrefix(err.Error(), "uploads: ") + " (" + envS3Endpoint + " / " + envS3Bucket + ")")
	}
	return s
}

// buildUploads is the upload service, or nil (feature off) — never an error.
func buildUploads(cfg config, pool *pgxpool.Pool, logger *log.Logger) *uploads.Service {
	s := parseUploads(cfg, pool != nil)
	if s.off != "" {
		return nil
	}
	objects, err := uploads.NewS3(s.s3)
	if err != nil {
		return nil // parseUploads has already built one from the same config
	}
	return &uploads.Service{Store: uploads.NewPG(pool), Objects: objects, Signer: s.signer,
		PublicURL: cfg.PublicURL, MaxBytes: s.maxBytes, LinkTTL: s.ttl, Logger: logger}
}

// checkUploadBucket reaches the bucket once, in the background, and logs the
// result: a wrong credential or a missing bucket is found at boot rather than
// by the first person who attaches a file. It does not turn the feature off —
// the store may simply not be up yet.
func checkUploadBucket(svc *uploads.Service, logger *log.Logger) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := svc.Objects.Check(ctx); err != nil {
			logger.Printf("uploads: WARNING — the bucket could not be reached at boot (%v); attaching a file "+
				"will fail until it can. Check %s, %s and the access key's policy", err, envS3Endpoint, envS3Bucket)
			return
		}
		logger.Printf("uploads: the bucket answered at boot")
	}()
}

// logUploadsBanner states the attachments' posture in both directions. It
// names the endpoint and bucket, never a key.
func (a *app) logUploadsBanner(ext api.Extensions) {
	names := strings.Join(uploadEnvNames, ", ")
	if ext.Uploads == nil {
		s := parseUploads(a.cfg, a.pool != nil)
		state := "OFF"
		if s.misconfigured {
			state = "OFF (MISCONFIGURED)"
		}
		why := s.off
		if why == "" {
			why = "the service was not wired"
		}
		a.logger.Printf("file attachments: %s — %s. No composer draws an attach button, and POST /uploads and "+
			"GET /f/{id} answer 404. Variables: %s", state, why, names)
		return
	}
	a.logger.Printf("file attachments: ON — files go to %s=%s bucket %s=%s (%s=%s); agents get links on %s "+
		"signed with %s (set), valid %s (%s), files up to %d bytes (%s). Also read: %s, %s",
		envS3Endpoint, a.cfg.S3Endpoint, envS3Bucket, a.cfg.S3Bucket, envS3Region, regionOrDefault(a.cfg.S3Region),
		envPublicURL, envUploadSigningKey, ext.Uploads.LinkTTL, envUploadLinkTTL, ext.Uploads.MaxBytes, envUploadMaxBytes,
		envS3AccessKey, envS3SecretKey)
}

func regionOrDefault(r string) string {
	if r == "" {
		return uploads.DefaultRegion
	}
	return r
}
