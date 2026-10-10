package k8s_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/client-go/rest"

	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/provision/k8s"
	"github.com/ZacxDev/muster/internal/provision/provisiontest"
)

// TestExecIsGatedOnTheRESTConfig.
//
// This file covers the DECISION around exec — that the capability follows the
// configuration, that provision.Exec refuses when it is off, and that the
// method returns a named error instead of dereferencing a nil config. The
// stream itself is exec_stream_test.go's, against an in-process endpoint that
// speaks the exec subresource's SPDY protocol; that file's header says what it
// still is not (an apiserver and a kubelet), so nothing here proves a command
// runs in a real pod.
func TestExecIsGatedOnTheRESTConfig(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) { testExecIsGatedOnTheRESTConfig(t, m) })
}

func testExecIsGatedOnTheRESTConfig(t *testing.T, m nsMode) {
	ctx := context.Background()

	without, _ := newDriver(t, m, nil)
	if without.Capabilities().Exec {
		t.Fatal("no RESTConfig must mean Capabilities.Exec false")
	}
	// The helper refuses before reaching the driver.
	err := provision.Exec(ctx, without, provision.Ref{Name: "anything"}, []string{"true"}, nil, nil, nil)
	if !errors.Is(err, provision.ErrUnsupported) {
		t.Fatalf("provision.Exec must refuse, got %v", err)
	}
	// And the method itself refuses rather than panicking, for a caller that
	// asserted the interface directly.
	err = without.Exec(ctx, provision.Ref{Name: "anything"}, []string{"true"}, nil, nil, nil)
	if !errors.Is(err, provision.ErrUnsupported) {
		t.Fatalf("Driver.Exec must refuse without a RESTConfig, got %v", err)
	}
	if !strings.Contains(err.Error(), "RESTConfig") {
		t.Fatalf("the refusal must name the missing configuration, got %q", err)
	}

	with, _ := newDriver(t, m, func(c *k8s.Config) {
		c.RESTConfig = &rest.Config{Host: "https://cluster.example.test"}
	})
	if !with.Capabilities().Exec {
		t.Fatal("a RESTConfig must mean Capabilities.Exec true")
	}
	// With the capability on, the refusal must come from the INSTANCE not
	// existing rather than from the configuration — which is what proves the
	// capability check above was the thing being measured.
	err = provision.Exec(ctx, with, provision.Ref{Name: "never-created"}, []string{"true"}, nil, nil, nil)
	if !errors.Is(err, provision.ErrNotFound) {
		t.Fatalf("want ErrNotFound for a missing instance, got %v", err)
	}

	// An existing instance with no pod is also not-found, and must not be an
	// empty success.
	spec := provisiontest.MinimalSpec("execless")
	if err := with.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	err = provision.Exec(ctx, with, spec.Ref, []string{"true"}, nil, nil, nil)
	if !errors.Is(err, provision.ErrNotFound) {
		t.Fatalf("an instance with no running pod must be ErrNotFound, got %v", err)
	}

	// An empty command is a caller bug, reported as one.
	if err := with.Exec(ctx, spec.Ref, nil, nil, nil, nil); !errors.Is(err, provision.ErrInvalidSpec) {
		t.Fatalf("want ErrInvalidSpec for an empty command, got %v", err)
	}
}
