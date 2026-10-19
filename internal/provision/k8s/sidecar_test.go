package k8s_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/provision/provisiontest"
)

// sidecarSpec is an instance with its own env, secret and files, and a sidecar
// with its own of each — every one distinct, so a value showing up on the
// wrong container is visible.
func sidecarSpec(name string) provision.Spec {
	spec := provisiontest.MinimalSpec(name)
	spec.Secrets = []provision.EnvVar{{Name: "MAIN_TOKEN", Value: "main-secret-value"}}
	spec.Files = []provision.File{{Path: "/etc/main/conf", Content: []byte("main")}}
	spec.Security = provision.Security{RunAsUser: 1000, RunAsGroup: 1000, FSGroup: 1000, RunAsNonRoot: true, Restricted: true}
	spec.Sidecar = &provision.Sidecar{
		Name:    "gateway",
		Runtime: provision.Runtime{Image: "ghcr.io/example-org/side:1", Command: []string{"sh", "-eu", "/srv/side/install"}, WorkingDir: "/data/side"},
		Env:     []provision.EnvVar{{Name: "SIDE_PORT", Value: "18791"}},
		Secrets: []provision.EnvVar{{Name: "SIDE_KEY", Value: "side-secret-value"}},
		Files: []provision.File{
			{Path: "/srv/side/install", Content: []byte("exec true\n")},
			{Path: "/srv/side/cred", Content: []byte("side-file-secret"), Secret: true},
		},
		Resources: provision.Resources{MemoryRequest: "256Mi", MemoryLimit: "1Gi"},
	}
	return spec
}

// TestASidecarRunsBesideTheInstanceWithOnlyItsOwnSecretsAndFiles: the pod has
// both containers; each mounts only its own files and envFroms only its own
// Secret (the sidecar's is a separate object holding none of the instance's);
// the sidecar does NOT mount the workspace, has the restricted security context,
// declares no port and carries its own resources. Then an Update without the
// sidecar removes the container AND sweeps its Secret.
func TestASidecarRunsBesideTheInstanceWithOnlyItsOwnSecretsAndFiles(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		d, cs := newDriver(t, m, nil)
		ctx := context.Background()
		spec := sidecarSpec("side")
		if err := d.Create(ctx, spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		ns := m.ns("side")
		dep, err := cs.AppsV1().Deployments(ns).Get(ctx, "side", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cs2 := dep.Spec.Template.Spec.Containers
		if len(cs2) != 2 || cs2[1].Name != "gateway" {
			t.Fatalf("containers: %d (%v); want the instance's and the sidecar", len(cs2), names(cs2))
		}
		main, side := cs2[0], cs2[1]
		mounts := func(c corev1.Container) map[string]bool {
			out := map[string]bool{}
			for _, vm := range c.VolumeMounts {
				out[vm.MountPath] = true
			}
			return out
		}
		mm, sm := mounts(main), mounts(side)
		if !mm["/etc/main/conf"] || mm["/srv/side/install"] || mm["/srv/side/cred"] {
			t.Errorf("the instance's mounts %v: want its own file and none of the sidecar's", mm)
		}
		if !sm["/srv/side/install"] || !sm["/srv/side/cred"] || sm["/etc/main/conf"] {
			t.Errorf("the sidecar's mounts %v: want its own files and none of the instance's", sm)
		}
		// NOT the workspace: the instance's volume is the instance's alone.
		for _, vm := range side.VolumeMounts {
			if vm.Name != "muster-files" && vm.Name != "muster-secret-files" {
				t.Errorf("the sidecar mounts volume %q at %s; it mounts its own files only", vm.Name, vm.MountPath)
			}
		}
		envFrom := func(c corev1.Container) []string {
			var out []string
			for _, e := range c.EnvFrom {
				out = append(out, e.SecretRef.Name)
			}
			return out
		}
		if got := envFrom(main); len(got) != 1 || got[0] != "side-env" {
			t.Errorf("the instance's envFrom %v, want only side-env", got)
		}
		if got := envFrom(side); len(got) != 1 || got[0] != "side-sidecar-env" {
			t.Errorf("the sidecar's envFrom %v, want only side-sidecar-env", got)
		}
		mainSec, err := cs.CoreV1().Secrets(ns).Get(ctx, "side-env", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		sideSec, err := cs.CoreV1().Secrets(ns).Get(ctx, "side-sidecar-env", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := mainSec.Data["SIDE_KEY"]; ok || string(mainSec.Data["MAIN_TOKEN"]) != "main-secret-value" {
			t.Errorf("the instance's Secret keys %v: want MAIN_TOKEN only", secretKeys(mainSec.Data))
		}
		if _, ok := sideSec.Data["MAIN_TOKEN"]; ok || string(sideSec.Data["SIDE_KEY"]) != "side-secret-value" {
			t.Errorf("the sidecar's Secret keys %v: want SIDE_KEY only", secretKeys(sideSec.Data))
		}
		for _, e := range side.Env {
			if strings.Contains(e.Value, "secret-value") {
				t.Errorf("a secret value is in the sidecar's plain env: %s", e.Name)
			}
		}
		if len(side.Ports) != 0 || side.StartupProbe != nil || side.LivenessProbe != nil {
			t.Errorf("the sidecar declares ports %v or probes; it is reached on loopback only", side.Ports)
		}
		if side.SecurityContext == nil || side.SecurityContext.AllowPrivilegeEscalation == nil || *side.SecurityContext.AllowPrivilegeEscalation {
			t.Errorf("the sidecar's security context %+v is not the instance's restricted one", side.SecurityContext)
		}
		if side.Resources.Limits.Memory().String() != "1Gi" || side.Image != "ghcr.io/example-org/side:1" ||
			strings.Join(side.Command, " ") != "sh -eu /srv/side/install" || side.WorkingDir != "/data/side" {
			t.Errorf("the sidecar container %+v is not the spec's", side)
		}

		// Without the sidecar: one container, and its Secret is swept.
		spec.Sidecar = nil
		if err := d.Update(ctx, spec); err != nil {
			t.Fatalf("Update: %v", err)
		}
		dep, _ = cs.AppsV1().Deployments(ns).Get(ctx, "side", metav1.GetOptions{})
		if n := len(dep.Spec.Template.Spec.Containers); n != 1 {
			t.Errorf("after dropping the sidecar: %d containers", n)
		}
		if _, err := cs.CoreV1().Secrets(ns).Get(ctx, "side-sidecar-env", metav1.GetOptions{}); err == nil {
			t.Error("the sidecar's Secret outlived the sidecar")
		}
	})
}

// TestTheSidecarMovesTheFingerprintOnlyWhenDeclared: a spec without one keeps
// its fingerprint (the sidecar is written only when present), and adding one
// or changing it moves it — so Start re-applying a chief's spec rolls its pod.
func TestTheSidecarMovesTheFingerprintOnlyWhenDeclared(t *testing.T) {
	spec := sidecarSpec("fp")
	with := provision.Fingerprint(spec)
	changed := sidecarSpec("fp")
	changed.Sidecar.Env = []provision.EnvVar{{Name: "SIDE_PORT", Value: "18792"}}
	spec.Sidecar = nil
	without := provision.Fingerprint(spec)
	plain := provisiontest.MinimalSpec("fp")
	plain.Secrets, plain.Files, plain.Security = spec.Secrets, spec.Files, spec.Security
	if without != provision.Fingerprint(plain) {
		t.Error("a nil sidecar changed the fingerprint")
	}
	if with == without || provision.Fingerprint(changed) == with {
		t.Error("adding or changing the sidecar did not move the fingerprint")
	}
}

// TestASidecarIsValidatedAndRefusedWhereItCannotRun.
func TestASidecarIsValidatedAndRefusedWhereItCannotRun(t *testing.T) {
	for _, c := range []struct {
		why string
		mut func(*provision.Spec)
	}{
		{"a name that is not a DNS label", func(s *provision.Spec) { s.Sidecar.Name = "Gate_way" }},
		{"no image", func(s *provision.Spec) { s.Sidecar.Runtime.Image = "" }},
		{"a relative file path", func(s *provision.Spec) { s.Sidecar.Files[0].Path = "srv/x" }},
		{"a file path the instance also uses", func(s *provision.Spec) { s.Sidecar.Files[0].Path = "/etc/main/conf" }},
	} {
		spec := sidecarSpec("v")
		c.mut(&spec)
		if err := spec.Validate(); !errors.Is(err, provision.ErrInvalidSpec) {
			t.Errorf("%s: Validate = %v, want ErrInvalidSpec", c.why, err)
		}
	}
	spec := sidecarSpec("v")
	if err := spec.Validate(); err != nil {
		t.Fatalf("control: the valid spec: %v", err)
	}
	caps := provision.DefaultNoopCapabilities
	caps.Sidecar = false
	if err := provision.CheckSpec(caps, spec); !errors.Is(err, provision.ErrUnsupported) {
		t.Errorf("a driver with no sidecar support: %v, want ErrUnsupported", err)
	}
	caps = provision.DefaultNoopCapabilities
	caps.Secrets = false
	spec.Secrets = nil
	if err := provision.CheckSpec(caps, spec); !errors.Is(err, provision.ErrUnsupported) || !strings.Contains(err.Error(), "sidecar") {
		t.Errorf("a sidecar secret on a driver with no confidential store: %v, want ErrUnsupported naming the sidecar", err)
	}
	// The instance's own container name is refused at render.
	eachMode(t, func(t *testing.T, m nsMode) {
		d, _ := newDriver(t, m, nil)
		bad := sidecarSpec("named")
		bad.Sidecar.Name = "agent"
		if err := d.Create(context.Background(), bad); !errors.Is(err, provision.ErrInvalidSpec) {
			t.Errorf("a sidecar named like the instance's container: %v", err)
		}
	})
}

func names(cs []corev1.Container) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func secretKeys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
