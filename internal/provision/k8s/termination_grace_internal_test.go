package k8s

import "testing"

// A declared termination grace is the pod's terminationGracePeriodSeconds; none
// declared renders none (the cluster's 30 s default, as before the field).
func TestTheTerminationGraceRendersIntoThePod(t *testing.T) {
	d, _ := internalDriver(t)
	spec := internalSpec("grace-otter")
	dep, err := d.renderDeployment(spec, internalNS)
	if err != nil {
		t.Fatal(err)
	}
	if g := dep.Spec.Template.Spec.TerminationGracePeriodSeconds; g != nil {
		t.Fatalf("no declared grace rendered %d", *g)
	}
	spec.TerminationGraceSeconds = 60
	dep, err = d.renderDeployment(spec, internalNS)
	if err != nil {
		t.Fatal(err)
	}
	if g := dep.Spec.Template.Spec.TerminationGracePeriodSeconds; g == nil || *g != 60 {
		t.Fatalf("a 60 s grace rendered %v", g)
	}
}
