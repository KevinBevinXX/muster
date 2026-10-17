package main

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/uploads"
	"github.com/ZacxDev/muster/internal/uploads/uploadstest"
)

// uploadEnv is a complete, valid upload configuration.
func uploadEnv() map[string]string {
	return map[string]string{
		envDatabase:         "postgres://x/y",
		envUIPassword:       strings.Repeat("p", 40),
		envPublicURL:        "https://muster.example.test",
		envS3Endpoint:       "http://minio.example.invalid:9000",
		envS3Bucket:         "muster-uploads",
		envS3AccessKey:      "access-fixture",
		envS3SecretKey:      "secret-fixture",
		envUploadSigningKey: strings.Repeat("k", 40),
	}
}

func uploadCfg(t *testing.T, env map[string]string) config {
	t.Helper()
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("loadConfig REFUSED to boot (%v); a bad upload configuration must turn the feature off, never the server", err)
	}
	return cfg
}

// TestUploadsAreOnOnlyWithTheWholeSet: the full set builds a service with the
// defaults (25 MiB, 24h) or the configured knobs.
func TestUploadsAreOnOnlyWithTheWholeSet(t *testing.T) {
	cfg := uploadCfg(t, uploadEnv())
	svc := buildUploads(cfg, &pgxpool.Pool{}, log.New(&bytes.Buffer{}, "", 0))
	if svc == nil {
		t.Fatalf("the full set left uploads off: %s", parseUploads(cfg, true).off)
	}
	if svc.MaxBytes != uploads.DefaultMaxBytes || svc.LinkTTL != uploads.DefaultLinkTTL || svc.PublicURL != "https://muster.example.test" {
		t.Errorf("defaults: max %d ttl %v url %q", svc.MaxBytes, svc.LinkTTL, svc.PublicURL)
	}
	env := uploadEnv()
	env[envUploadMaxBytes], env[envUploadLinkTTL] = "1048576", "2h"
	svc = buildUploads(uploadCfg(t, env), &pgxpool.Pool{}, log.New(&bytes.Buffer{}, "", 0))
	if svc == nil || svc.MaxBytes != 1<<20 || svc.LinkTTL.Hours() != 2 {
		t.Errorf("the knobs were not applied: %+v", svc)
	}
	// Unset entirely: plain OFF, not MISCONFIGURED.
	off := parseUploads(uploadCfg(t, map[string]string{envDatabase: "postgres://x/y", envUIPassword: strings.Repeat("p", 40)}), true)
	if off.off == "" || off.misconfigured {
		t.Errorf("nothing set: %+v; want off and not misconfigured", off)
	}
}

// TestAMisconfiguredSetIsOffNotARefusal: each half-configuration boots, builds
// NO service, and says MISCONFIGURED naming the thing to fix.
func TestAMisconfiguredSetIsOffNotARefusal(t *testing.T) {
	for name, tc := range map[string]struct {
		mut  func(map[string]string)
		db   bool
		name string // what the reason must name
	}{
		"no signing key":     {func(e map[string]string) { delete(e, envUploadSigningKey) }, true, envUploadSigningKey},
		"short signing key":  {func(e map[string]string) { e[envUploadSigningKey] = "short" }, true, envUploadSigningKey},
		"no secret key":      {func(e map[string]string) { delete(e, envS3SecretKey) }, true, envS3SecretKey},
		"no bucket":          {func(e map[string]string) { delete(e, envS3Bucket) }, true, envS3Bucket},
		"bad bucket":         {func(e map[string]string) { e[envS3Bucket] = "Not_A_Bucket" }, true, envS3Bucket},
		"bad endpoint":       {func(e map[string]string) { e[envS3Endpoint] = "minio:9000" }, true, envS3Endpoint},
		"no public url":      {func(e map[string]string) { delete(e, envPublicURL) }, true, envPublicURL},
		"no database":        {func(map[string]string) {}, false, envDatabase},
		"max bytes not a no": {func(e map[string]string) { e[envUploadMaxBytes] = "25MB" }, true, envUploadMaxBytes},
		"max bytes too big":  {func(e map[string]string) { e[envUploadMaxBytes] = "999999999999" }, true, envUploadMaxBytes},
		"ttl too long":       {func(e map[string]string) { e[envUploadLinkTTL] = "720h" }, true, envUploadLinkTTL},
		"ttl not a duration": {func(e map[string]string) { e[envUploadLinkTTL] = "a day" }, true, envUploadLinkTTL},
	} {
		env := uploadEnv()
		tc.mut(env)
		cfg := uploadCfg(t, env)
		var pool *pgxpool.Pool
		if tc.db {
			pool = &pgxpool.Pool{}
		}
		if svc := buildUploads(cfg, pool, log.New(&bytes.Buffer{}, "", 0)); svc != nil {
			t.Errorf("%s: a service was built", name)
		}
		s := parseUploads(cfg, tc.db)
		if !s.misconfigured || !strings.Contains(s.off, tc.name) {
			t.Errorf("%s: %+v; want MISCONFIGURED naming %s", name, s, tc.name)
		}
		var buf bytes.Buffer
		a := &app{cfg: cfg, logger: log.New(&buf, "", 0), pool: pool}
		a.logUploadsBanner(api.Extensions{})
		if line := buf.String(); !strings.Contains(line, "file attachments: OFF (MISCONFIGURED)") || !strings.Contains(line, tc.name) {
			t.Errorf("%s: banner %q", name, line)
		}
	}
}

// TestTheUploadBannerNeverPrintsAKey: neither S3 key nor the signing key is in
// the ON line.
func TestTheUploadBannerNeverPrintsAKey(t *testing.T) {
	env := uploadEnv()
	cfg := uploadCfg(t, env)
	var buf bytes.Buffer
	a := &app{cfg: cfg, logger: log.New(&buf, "", 0), pool: &pgxpool.Pool{}}
	a.logUploadsBanner(api.Extensions{Uploads: buildUploads(cfg, a.pool, a.logger)})
	line := buf.String()
	if !strings.Contains(line, "file attachments: ON") {
		t.Fatalf("banner = %q", line)
	}
	for _, secret := range []string{env[envS3SecretKey], env[envUploadSigningKey], env[envS3AccessKey]} {
		if strings.Contains(line, secret) {
			t.Errorf("the banner prints a credential (%q): %s", secret, line)
		}
	}
}

// TestTheBootCheckWarnsWhenNothingExpiresTheUploads: a bucket with no rule
// expiring uploads/ grows for ever (an expired link leaves its file behind),
// and one with no rule aborting incomplete uploads keeps a killed upload's
// parts — the boot log says each; a key that may not read the lifecycle says
// THAT, rather than claiming either answer.
func TestTheBootCheckWarnsWhenNothingExpiresTheUploads(t *testing.T) {
	expire := "NO lifecycle rule expiring uploads/"
	abort := "NO lifecycle rule aborting incomplete multipart uploads"
	for name, tc := range map[string]struct {
		cov     uploads.LifecycleCoverage
		err     error
		want    []string
		wantNot []string
	}{
		"no rule":     {uploads.LifecycleCoverage{}, nil, []string{expire, abort}, nil},
		"expiry only": {uploads.LifecycleCoverage{Expire: 1}, nil, []string{abort}, []string{expire}},
		"abort only":  {uploads.LifecycleCoverage{AbortIncomplete: 1}, nil, []string{expire}, []string{abort}},
		"both":        {uploads.LifecycleCoverage{Expire: 1, AbortIncomplete: 1}, nil, []string{"expires uploads/ objects (1 rule(s)) and aborts incomplete uploads (1 rule(s))"}, []string{expire, abort}},
		"unreadable":  {uploads.LifecycleCoverage{}, uploads.ErrLifecycleUnreadable, []string{"could NOT be read with this key"}, []string{expire, abort}},
	} {
		var buf bytes.Buffer
		logUploadBucketCheck(context.Background(), &uploadstest.Objects{Coverage: tc.cov, RulesErr: tc.err}, log.New(&buf, "", 0))
		for _, w := range tc.want {
			if !strings.Contains(buf.String(), w) {
				t.Errorf("%s: logged %q; want %q", name, buf.String(), w)
			}
		}
		for _, w := range tc.wantNot {
			if strings.Contains(buf.String(), w) {
				t.Errorf("%s: logged %q, which it should not", name, w)
			}
		}
	}
}
