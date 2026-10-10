package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/ZacxDev/muster/internal/provision"
)

// PodExecRBACPrerequisite is every verb the muster ServiceAccount must hold on
// the `pods/exec` subresource for Exec and ExecTTY to work. Both open the exec
// stream with an SPDY upgrade, which is a POST, and the apiserver authorises a
// POST to a subresource as `create` — nothing else is called on it.
// TestTheExecRBACPrerequisiteIsTheVerbTheDriverSends pins it to the method the
// driver actually sends.
//
// ⚠ A WEBSOCKET exec (kubectl's default since 1.30) is a GET, authorised as
// `get` on older apiservers and as `create` where the
// AuthorizePodWebsocketUpgradeCreatePermission gate is on. This driver does not
// use it, so `get` is not part of the prerequisite; adding the WebSocket
// executor would change that, and this list with it.
//
// As a ClusterRole rule:
//
//   - apiGroups: [""]
//     resources: ["pods/exec"]
//     verbs: ["create"]
var PodExecRBACPrerequisite = []string{"create"}

// execMethod is the HTTP method the exec stream is opened with. It is what
// PodExecRBACPrerequisite is derived from, so it is named once.
const execMethod = "POST"

// podExecRBACHint is the rule to add, as one sentence for an error message.
var podExecRBACHint = "grant muster's ClusterRole apiGroups [\"\"] resources [\"pods/exec\"] verbs [" +
	quotedVerbs(PodExecRBACPrerequisite) + "]"

// Exec implements provision.Execer.
//
// ⚠ ITS STREAM IS COVERED NOW, BY THE SAME IN-PROCESS EXEC SERVER AS ExecTTY's
// (exec_stream_test.go), WHICH SPEAKS THE APISERVER'S SPDY PROTOCOL. This doc
// used to say the method was uncovered because client-go's fake clientset does
// not serve the subresource — true of the fake, and the reason the request is
// now built from RESTConfig rather than from Config.Client: the fake's REST
// client is nil, so a request built from it could not be pointed at a test
// server at all. What is still NOT covered is a real apiserver and kubelet.
//
// ⚠ A RELATED TRAP, RECORDED BECAUSE IT COST A WHOLE INVESTIGATION IN THE
// PROJECT THIS CAME FROM: `kubectl auth can-i get pods/log` answered YES for a
// service account whose real request then got 403. Do not verify exec or log
// access with an access review — make the call with the real token.
func (d *Driver) Exec(ctx context.Context, ref provision.Ref, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(cmd) == 0 {
		return fmt.Errorf("%w: exec needs a command", provision.ErrInvalidSpec)
	}
	exec, err := d.execStream(ctx, ref, &corev1.PodExecOptions{
		Command: cmd,
		Stdin:   stdin != nil,
		Stdout:  stdout != nil,
		Stderr:  stderr != nil,
	})
	if err != nil {
		return err
	}
	return execError(ref, exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	}))
}

// ExecTTY implements provision.TTYExecer: the command runs on a pseudo-terminal
// in the instance's container, with stderr merged into stdout by the terminal
// and every size from resize applied to it.
//
// 🔴 stdin IS ALWAYS ATTACHED, EVEN WHEN THE CALLER NEVER WRITES TO IT. A
// read-only viewer still needs a terminal whose input is open, because a
// process that sees its stdin close may exit (tmux's client does), and the
// caller decides what reaches stdin — not this method.
func (d *Driver) ExecTTY(ctx context.Context, ref provision.Ref, cmd []string, stdin io.Reader, stdout io.Writer, resize <-chan provision.TermSize) error {
	if len(cmd) == 0 {
		return fmt.Errorf("%w: a terminal needs a command", provision.ErrInvalidSpec)
	}
	if stdin == nil || stdout == nil {
		return fmt.Errorf("%w: a terminal needs both stdin and stdout", provision.ErrInvalidSpec)
	}
	// Scoped to this call: client-go reads the size queue on its own goroutine,
	// and sizeQueue.Next only returns when its context ends. Without this the
	// goroutine would outlive a command that exited on its own until the CALLER
	// got round to cancelling.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	exec, err := d.execStream(ctx, ref, &corev1.PodExecOptions{
		Command: cmd,
		Stdin:   true,
		Stdout:  true,
		// A TTY merges stderr into stdout; the apiserver refuses both at once.
		Stderr: false,
		TTY:    true,
	})
	if err != nil {
		return err
	}
	opts := remotecommand.StreamOptions{Stdin: stdin, Stdout: stdout, Tty: true}
	if resize != nil {
		opts.TerminalSizeQueue = sizeQueue{ctx: ctx, ch: resize}
	}
	return execError(ref, exec.StreamWithContext(ctx, opts))
}

// execStream resolves the instance's pod and builds the exec stream for it.
func (d *Driver) execStream(ctx context.Context, ref provision.Ref, opts *corev1.PodExecOptions) (remotecommand.Executor, error) {
	if d.cfg.RESTConfig == nil {
		return nil, fmt.Errorf("%w: this driver was built without a RESTConfig, so it cannot exec "+
			"(Capabilities.Exec is false and provision.Exec refuses before reaching here)", provision.ErrUnsupported)
	}
	ns, pod, err := d.podFor(ctx, ref)
	if err != nil {
		return nil, err
	}
	if pod == "" {
		return nil, fmt.Errorf("%w: %q has no running pod to exec into", provision.ErrNotFound, ref.Name)
	}
	// Named, for the same reason the log reads are. An unnamed container
	// silently means "the first one".
	opts.Container = containerName
	u, err := execURL(d.cfg.RESTConfig, ns, pod, opts)
	if err != nil {
		return nil, fmt.Errorf("exec into %s: %w", ref.Name, err)
	}
	exec, err := remotecommand.NewSPDYExecutor(d.cfg.RESTConfig, execMethod, u)
	if err != nil {
		return nil, fmt.Errorf("exec into %s: %w", ref.Name, err)
	}
	return exec, nil
}

// execURL is the pods/exec URL for pod, built from the REST config the stream
// itself is opened with — so the request and the stream can never name two
// different apiservers.
func execURL(rc *rest.Config, ns, pod string, opts *corev1.PodExecOptions) (*url.URL, error) {
	cfg := rest.CopyConfig(rc)
	cfg.GroupVersion = &corev1.SchemeGroupVersion
	cfg.APIPath = "/api"
	cfg.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	if cfg.UserAgent == "" {
		cfg.UserAgent = rest.DefaultKubernetesUserAgent()
	}
	client, err := rest.RESTClientFor(cfg)
	if err != nil {
		return nil, err
	}
	return client.Post().
		Resource("pods").
		Name(pod).
		Namespace(ns).
		SubResource("exec").
		VersionedParams(opts, scheme.ParameterCodec).
		URL(), nil
}

// execError names an apiserver refusal of the exec subresource.
//
// 🔴 A 403 HERE IS ALMOST ALWAYS MISSING RBAC, AND IT MUST SAY WHICH RULE. The
// upgrade answers before any stream exists, so a caller sees it immediately —
// and without the hint the browser shows "forbidden" for a terminal whose fix
// is one line in a ClusterRole nobody connects to it. The sentinel is
// ErrUnsupported, as for the NetworkPolicy refusal: the backend answered, and
// what it said is that muster may not do this.
func execError(ref provision.Ref, err error) error {
	if err == nil {
		return nil
	}
	if apierrors.IsForbidden(err) {
		return fmt.Errorf("%w: the cluster refused to exec into %s: %v — this is almost always RBAC: %s",
			provision.ErrUnsupported, ref.Name, err, podExecRBACHint)
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	return fmt.Errorf("exec into %s: %w", ref.Name, err)
}

// sizeQueue adapts a TermSize channel to client-go's TerminalSizeQueue.
//
// Next returning nil ends client-go's resize loop, so it does exactly that
// when ctx ends — the loop must not outlive the stream it feeds.
type sizeQueue struct {
	ctx context.Context
	ch  <-chan provision.TermSize
}

func (q sizeQueue) Next() *remotecommand.TerminalSize {
	select {
	case <-q.ctx.Done():
		return nil
	case s, ok := <-q.ch:
		if !ok {
			return nil
		}
		return &remotecommand.TerminalSize{Width: s.Cols, Height: s.Rows}
	}
}
