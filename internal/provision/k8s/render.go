package k8s

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/ZacxDev/muster/internal/provision"
)

// The label and annotation keys this driver writes.
//
// 🔴 `managed-by` PLUS `name` IS THE PAIR THAT SEPARATES THIS DRIVER'S OBJECTS
// FROM EVERYTHING ELSE IN A NAMESPACE, AND IT IS READ IN TWO DIFFERENT WAYS.
// managedSelector() is the server-side selector every enumeration passes;
// owned() is the client-side predicate every BY-NAME path checks before it
// reads, writes or deletes an object. Both are derived from managedLabels
// below, so the set of objects List reports and the set Destroy may touch
// cannot drift apart.
//
// ⚠ A NAME IS NOT AN IDENTITY. In a shared namespace anything can already hold
// the name an instance asks for, and acting on it by name alone is how a
// teardown deletes a stranger's Service and a status read reports somebody
// else's Deployment as a muster instance.
const (
	labelName      = "app.kubernetes.io/name"
	labelInstance  = "app.kubernetes.io/instance"
	labelManagedBy = "app.kubernetes.io/managed-by"

	managedBy  = "muster"
	nameValue  = "muster-agent"
	fieldOwner = "muster"

	// annFingerprint records provision.Fingerprint(spec) so Create can answer
	// the divergence question without storing the spec anywhere else.
	annFingerprint = "muster.dev/spec-fingerprint"
	// annCorrelationID records Ref.ID, so an operator reading the object can
	// find the row it came from. It is written only when non-zero — an
	// annotation reading "0" is a value, and this one means "there was none".
	annCorrelationID = "muster.dev/correlation-id"
	// annEndpoint records the per-instance Spec.Endpoint override as JSON, so
	// Endpoint() can honour it later without the spec.
	annEndpoint = "muster.dev/endpoint"
)

// AnnotationPort records the resolved reachable port, for the same reason as
// annEndpoint: so Endpoint() can answer later without the spec.
//
// 🔴 IT IS ON THE **DEPLOYMENT OBJECT**, NOT ON THE POD TEMPLATE, AND THIS COMMENT
// IS THE ONE PLACE THAT FACT IS STATED. renderDeployment puts
// renderAnnotations(spec) on the Deployment's own ObjectMeta; the pod template gets
// annFingerprint ALONE. Endpoint() reads `dep.Annotations[AnnotationPort]`, i.e. the
// Deployment, so these two kubectl paths are not interchangeable — measured off one
// rendered object in one run:
//
//	.spec.template.metadata.annotations["muster.dev/port"]  ->  ""
//	.metadata.annotations["muster.dev/port"]                ->  "18789"
//
// 🔴 WHICH IS WHY IT IS EXPORTED, AND THE EXPORT IS THE POINT RATHER THAN
// CONVENIENCE. The fact was open-coded in SIX places — a boot log line, three
// comments and two runnable `kubectl -o jsonpath=` recipes — and was wrong in the
// same direction in every one of them: they all named the pod template. A
// correctly-provisioned instance therefore printed EMPTY for the recipe that was
// supposed to confirm it, under an instruction to STOP if the output was empty. One
// predicate open-coded at N sites is usually wrong at N-1; this one was wrong at N.
// Anything that needs to name this annotation names THIS constant, and
// TestTheRecipesNameTheAnnotationEndpointActuallyReads in cmd/muster-server pins the
// jsonpath in the prose against a rendered object.
const AnnotationPort = "muster.dev/port"

// containerName is the single application container's name. It is a CONSTANT
// so that every log read, exec and status inspection names the same container
// explicitly — the project this replaces never named one, so every one of those
// operations silently read whichever container happened to be first.
const containerName = "agent"

// initContainerName runs Spec.Init.
const initContainerName = "muster-init"

// workspaceVolumeName is the shared volume the init container and the
// application container both see. It exists even for a non-persistent
// workspace, as an emptyDir, because otherwise an init step's output would not
// survive into the container that needs it — which is the single most common
// way an init container is written and silently does nothing.
const workspaceVolumeName = "workspace"

const (
	filesVolumeName       = "muster-files"
	secretFilesVolumeName = "muster-secret-files"
)

// DefaultWorkspacePath is where a workspace is mounted when the spec does not
// say.
const DefaultWorkspacePath = "/data/workspace"

// The probe timings this driver translates [provision.Health] into.
//
// 🔴 THEY LIVE HERE RATHER THAN ON provision.Health BECAUSE THEY ARE SPELLED IN
// THIS BACKEND'S UNITS. A period and a failure threshold are a Kubernetes shape;
// another backend expresses supervision as a retry budget, a restart policy or
// nothing at all. What the SPEC declares is the portable half — "GET this path on
// this port means serving" — and translating it is the driver's job. That is the
// same division [provision.Resources] draws by keeping quantities as strings.
//
// 🔴 THE NUMBERS ARE TAKEN FROM A DEPLOYMENT OF THE SAME AGENT RUNTIME IMAGE THAT
// IS KNOWN TO WORK, NOT CHOSEN. That deployment's gateway container carries a
// startup probe of GET / on the gateway port with failureThreshold 30, period 10s
// and initialDelay 5s, and a liveness probe of the same request with
// failureThreshold 3 and period 30s. Measured against the image muster's own
// deployment pins: a cold container reached `[gateway] ready` 1.3s after start, so
// the 305s startup budget is roughly 200x the observed need — deliberately, because
// the budget has to cover a loaded node and a cold page cache, and the cost of it
// being too generous is a slower crash report while the cost of it being too tight
// is a restart loop that never lets a healthy agent finish booting.
//
// ⚠ A STARTUP PROBE IS WHAT MAKES THE LIVENESS PROBE SAFE, AND THE PAIR MUST MOVE
// TOGETHER. Kubernetes suspends liveness until startup succeeds; a liveness probe
// with no startup probe would kill every container that takes longer than
// livenessFailureThreshold x livenessPeriodSeconds to boot, which is 90s here and
// is well inside the range an image pull plus a repository clone can reach.
const (
	startupInitialDelaySeconds = 5
	startupPeriodSeconds       = 10
	startupFailureThreshold    = 30

	livenessPeriodSeconds    = 30
	livenessFailureThreshold = 3
)

// ⚠ THERE IS NO READINESS PROBE, AND ITS ABSENCE IS A DECISION RATHER THAN THE
// SET THE REFERENCE DEPLOYMENT HAPPENED TO HAVE.
//
// The startup probe already gates the container's FIRST Ready — Kubernetes holds
// a container unready until startup succeeds — which is the signal muster needs:
// provision.Instance.Ready is what tells an operator a provisioned agent came up
// at all, and it is the mechanical half of this change's closing condition. What a
// readiness probe would add on top is continuous REMOVAL from the Service after
// startup, on the identical request, which for a single-replica Deployment serving
// chat means one slow response takes the agent out of its own Service mid-turn and
// the chat failure reads as a muster defect. The hang case that argument gives up
// is covered by liveness, which restarts rather than hides.
//
// 🔴 IF A SECOND REPLICA EVER BECOMES POSSIBLE, RE-ARGUE THIS. The reasoning above
// is specific to replicas: 1, where "pulled from the Service" and "unreachable"
// are the same thing. With two replicas a readiness probe is how traffic avoids a
// sick one, and its absence becomes a defect rather than a choice.
func healthProbe(spec provision.Spec) *corev1.Probe {
	if spec.Health.IsZero() {
		return nil
	}
	port := 0
	for _, p := range spec.Ports {
		if p.Name == spec.Health.PortName {
			port = p.Port
			break
		}
	}
	if port == 0 {
		// Unreachable through Validate, which refuses a Health naming an
		// undeclared port. Returning nil rather than probing a guessed port is
		// the honest half of that: a probe against the wrong port reports an
		// instance unhealthy for a reason nothing logs.
		return nil
	}
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: spec.Health.HTTPGetPath,
				Port: intstr.FromInt32(int32(port)),
			},
		},
	}
}

// startupProbe and livenessProbe are the same request with different budgets.
//
// 🔴 THEY ARE BUILT FROM SEPARATE CALLS TO healthProbe RATHER THAN FROM ONE VALUE
// COPIED TWICE. A *corev1.Probe shared between the two fields would be ONE object
// reachable from two places in the pod spec, so setting a threshold on one would
// set it on both — and the pod would apply, with a liveness probe carrying the
// startup budget (305s to notice a dead gateway) or a startup probe carrying the
// liveness one (90s to boot). Both are silent.
func startupProbe(spec provision.Spec) *corev1.Probe {
	p := healthProbe(spec)
	if p == nil {
		return nil
	}
	p.InitialDelaySeconds = startupInitialDelaySeconds
	p.PeriodSeconds = startupPeriodSeconds
	p.FailureThreshold = startupFailureThreshold
	return p
}

func livenessProbe(spec provision.Spec) *corev1.Probe {
	p := healthProbe(spec)
	if p == nil {
		return nil
	}
	p.PeriodSeconds = livenessPeriodSeconds
	p.FailureThreshold = livenessFailureThreshold
	return p
}

// DefaultEndpointTemplate is the in-cluster address of an instance's Service.
//
// It is a DEFAULT, not a constant in the code path: Config.EndpointTemplate
// replaces it wholesale and Spec.Endpoint overrides it per instance. The string
// it replaces in the original project was reachable from nowhere.
//
// ⚠ IT STOPS AT `.svc` DELIBERATELY. A pod's own resolv.conf search path
// supplies the cluster's DNS domain, whatever that cluster was configured with,
// so `name.namespace.svc` resolves in-cluster everywhere. Spelling the default
// domain in full would resolve only on a cluster that kept the default, and
// neither form resolves from outside the cluster at all — so the short one
// loses nothing and survives a cluster whose clusterDomain was changed.
const DefaultEndpointTemplate = "{{.Name}}.{{.Group}}.svc"

// managedLabels are the labels every object this driver creates carries, and
// the single place the ownership rule is stated. managedSelector and owned are
// both derived from it: a selector that asked for one set while the predicate
// admitted another is a disagreement nothing would report.
var managedLabels = map[string]string{
	labelManagedBy: managedBy,
	labelName:      nameValue,
}

func instanceLabels(name string) map[string]string {
	out := map[string]string{labelInstance: name}
	for k, v := range managedLabels {
		out[k] = v
	}
	return out
}

// managedSelector is the label selector every enumeration uses.
func managedSelector() string {
	keys := make([]string, 0, len(managedLabels))
	for k := range managedLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+managedLabels[k])
	}
	return strings.Join(parts, ",")
}

// owned reports whether an object's labels mark it as one this driver created.
//
// 🔴 IT IS THE CLIENT-SIDE HALF OF managedSelector, AND IT IS WHAT EVERY
// BY-NAME PATH CHECKS. A List filters server-side; a Get, an Update or a Delete
// of a named object does not, so without this check the driver would report —
// and destroy — workloads it did not create, which is precisely what the labels
// exist to prevent.
func owned(labels map[string]string) bool {
	for k, v := range managedLabels {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func instanceSelector(name string) string {
	return managedSelector() + "," + labelInstance + "=" + name
}

// mergeLabels folds the operator's own labels under the driver's.
//
// ⚠ THE DRIVER'S LABELS WIN. They are the selector; letting a caller overwrite
// `app.kubernetes.io/instance` would point one instance's Deployment at another
// instance's pods, and the Deployment controller would happily adopt them.
func mergeLabels(own map[string]string, extra map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range extra {
		out[k] = v
	}
	for k, v := range own {
		out[k] = v
	}
	return out
}

// fileKey is the ConfigMap/Secret key holding one file's content.
//
// It is derived from the PATH, not from the file's position in the list, so
// reordering Spec.Files does not rewrite every key and roll the pod. The digest
// prefix makes it collision-free; the sanitised basename is there so a human
// reading `kubectl describe` can tell which file it is.
func fileKey(p string) string {
	sum := sha256.Sum256([]byte(p))
	base := path.Base(p)
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := b.String()
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	const maxBase = 40
	if len(name) > maxBase {
		name = name[:maxBase]
	}
	return hex.EncodeToString(sum[:6]) + "-" + name
}

// The names of the per-instance objects this driver renders.
//
// ⚠ THEY ARE FUNCTIONS, IN ONE PLACE, BECAUSE THREE DIFFERENT CODE PATHS HAVE
// TO AGREE ON THEM: the render, the stale-object sweep in apply, and Destroy.
// An object named one way at creation and another at teardown outlives the
// instance, and nothing reports that — it is simply still there.
func configMapName(instance string) string { return instance + "-files" }

func envSecretName(instance string) string { return instance + "-env" }

func fileSecretName(instance string) string { return instance + "-secret-files" }

// sidecarEnvSecretName holds the SIDECAR's confidential environment: its own
// object, envFrom'd by the sidecar container alone, so neither process is
// handed the other's credentials (provision.Sidecar).
func sidecarEnvSecretName(instance string) string { return instance + "-sidecar-env" }

// allFiles is every file the instance's pod places: its own and its sidecar's.
// They share the two file objects (keys are derived from distinct paths —
// Spec.Validate refuses a repeat); each container MOUNTS only its own.
func allFiles(spec provision.Spec) []provision.File {
	if spec.Sidecar == nil || len(spec.Sidecar.Files) == 0 {
		return spec.Files
	}
	return append(append([]provision.File(nil), spec.Files...), spec.Sidecar.Files...)
}

func pvcName(instance string) string { return instance + "-workspace" }

// sortedFiles returns the spec's files split into non-secret and secret,
// each sorted by path so every render is byte-stable.
func sortedFiles(files []provision.File) (plain, secret []provision.File) {
	for _, f := range files {
		if f.Secret {
			secret = append(secret, f)
		} else {
			plain = append(plain, f)
		}
	}
	sort.Slice(plain, func(i, j int) bool { return plain[i].Path < plain[j].Path })
	sort.Slice(secret, func(i, j int) bool { return secret[i].Path < secret[j].Path })
	return plain, secret
}

// --------------------------------------------------------------------------
// Object rendering. Each function is pure: spec in, object out. That is what
// makes the manifest set testable without a cluster, which is the property the
// chart it replaces did not have.
// --------------------------------------------------------------------------

func (d *Driver) renderServiceAccount(spec provision.Spec, ns string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Ref.Name,
			Namespace: ns,
			Labels:    mergeLabels(instanceLabels(spec.Ref.Name), spec.Labels),
		},
	}
}

// renderConfigMap holds the non-secret files. Returns nil when there are none,
// so the driver does not create an empty object it would then have to delete.
func (d *Driver) renderConfigMap(spec provision.Spec, ns string) *corev1.ConfigMap {
	plain, _ := sortedFiles(allFiles(spec))
	if len(plain) == 0 {
		return nil
	}
	data := map[string][]byte{}
	for _, f := range plain {
		data[fileKey(f.Path)] = f.Content
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName(spec.Ref.Name),
			Namespace: ns,
			Labels:    mergeLabels(instanceLabels(spec.Ref.Name), spec.Labels),
		},
		// BinaryData, not Data: provision.File carries bytes, and Data is a
		// string map that silently mangles anything that is not valid UTF-8.
		BinaryData: data,
	}
}

// renderEnvSecret holds the instance's confidential ENVIRONMENT, and nothing
// else. Returns nil when the spec declares none.
//
// 🔴 CONFIDENTIAL FILES ARE IN A DIFFERENT OBJECT, AND THE SPLIT IS THE WHOLE
// POINT OF THESE TWO FUNCTIONS BEING TWO. This Secret is consumed with
// `envFrom`, which turns EVERY key of it into an environment variable. A secret
// FILE sharing the object would therefore be exported into the process
// environment — readable from /proc/self/environ and inherited by every child —
// while the mounted copy's 0600 mode says nothing about that. Key names that
// are not valid environment variable names are skipped by the kubelet with an
// InvalidEnvironmentVariableNames warning instead, so the same defect reads as
// pod noise for some paths and as a leak for others.
func (d *Driver) renderEnvSecret(spec provision.Spec, ns string) *corev1.Secret {
	if len(spec.Secrets) == 0 {
		return nil
	}
	data := map[string][]byte{}
	for _, e := range spec.Secrets {
		data[e.Name] = []byte(e.Value)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      envSecretName(spec.Ref.Name),
			Namespace: ns,
			Labels:    mergeLabels(instanceLabels(spec.Ref.Name), spec.Labels),
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}

// renderFileSecret holds the instance's confidential FILES, and nothing else.
// Returns nil when the spec declares none.
//
// It is MOUNTED, never envFrom'd — see renderEnvSecret for why the two must not
// be one object.
func (d *Driver) renderFileSecret(spec provision.Spec, ns string) *corev1.Secret {
	_, secretFiles := sortedFiles(allFiles(spec))
	if len(secretFiles) == 0 {
		return nil
	}
	data := map[string][]byte{}
	for _, f := range secretFiles {
		data[fileKey(f.Path)] = f.Content
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fileSecretName(spec.Ref.Name),
			Namespace: ns,
			Labels:    mergeLabels(instanceLabels(spec.Ref.Name), spec.Labels),
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}

// renderSidecarEnvSecret is renderEnvSecret for the sidecar's own Secrets
// (sidecarEnvSecretName). Returns nil when there is no sidecar or it declares
// none.
func (d *Driver) renderSidecarEnvSecret(spec provision.Spec, ns string) *corev1.Secret {
	if spec.Sidecar == nil || len(spec.Sidecar.Secrets) == 0 {
		return nil
	}
	data := map[string][]byte{}
	for _, e := range spec.Sidecar.Secrets {
		data[e.Name] = []byte(e.Value)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sidecarEnvSecretName(spec.Ref.Name),
			Namespace: ns,
			Labels:    mergeLabels(instanceLabels(spec.Ref.Name), spec.Labels),
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}

func (d *Driver) renderPVC(spec provision.Spec, ns string) (*corev1.PersistentVolumeClaim, error) {
	if !spec.Workspace.Persist {
		return nil, nil
	}
	size := spec.Workspace.Size
	if size == "" {
		size = "10Gi"
	}
	qty, err := resource.ParseQuantity(size)
	if err != nil {
		return nil, fmt.Errorf("%w: workspace size %q: %v", provision.ErrInvalidSpec, size, err)
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName(spec.Ref.Name),
			Namespace: ns,
			Labels:    mergeLabels(instanceLabels(spec.Ref.Name), spec.Labels),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
			},
		},
	}
	// 🔴 A NIL STORAGE CLASS AND AN EMPTY ONE ARE DIFFERENT REQUESTS, AND THIS
	// IS THE ONE PLACE THE DISTINCTION IS LOAD-BEARING. Empty string means "use
	// the cluster's default StorageClass"; leaving the field unset entirely
	// means the same thing on most clusters but is expressed differently in the
	// object. Config.WorkspaceStorageClass is a *string precisely so an
	// operator can say "" and mean the default, rather than having "" mean "I
	// did not configure this" — the distinction nothing in the original project
	// could make anywhere.
	if d.cfg.WorkspaceStorageClass != nil && *d.cfg.WorkspaceStorageClass != "" {
		sc := *d.cfg.WorkspaceStorageClass
		pvc.Spec.StorageClassName = &sc
	}
	return pvc, nil
}

func (d *Driver) renderService(spec provision.Spec, ns string) *corev1.Service {
	if len(spec.Ports) == 0 {
		return nil
	}
	ports := make([]corev1.ServicePort, 0, len(spec.Ports))
	for _, p := range spec.Ports {
		proto := corev1.ProtocolTCP
		if strings.EqualFold(p.Protocol, "UDP") {
			proto = corev1.ProtocolUDP
		}
		name := p.Name
		if name == "" {
			name = "port-" + strconv.Itoa(p.Port)
		}
		ports = append(ports, corev1.ServicePort{
			Name:       name,
			Port:       int32(p.Port),
			TargetPort: intstr.FromInt32(int32(p.Port)),
			Protocol:   proto,
		})
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Ref.Name,
			Namespace: ns,
			Labels:    mergeLabels(instanceLabels(spec.Ref.Name), spec.Labels),
		},
		Spec: corev1.ServiceSpec{
			Selector: instanceLabels(spec.Ref.Name),
			Ports:    ports,
		},
	}
}

func (d *Driver) renderDeployment(spec provision.Spec, ns string) (*appsv1.Deployment, error) {
	labels := mergeLabels(instanceLabels(spec.Ref.Name), spec.Labels)

	env := make([]corev1.EnvVar, 0, len(spec.Env)+4)
	for _, e := range spec.Env {
		env = append(env, corev1.EnvVar{Name: e.Name, Value: e.Value})
	}
	// The repository is DECLARED here, not cloned. See the package doc.
	if spec.Repo.URL != "" {
		env = append(env, corev1.EnvVar{Name: "MUSTER_REPO_URL", Value: spec.Repo.URL})
		if spec.Repo.Branch != "" {
			env = append(env, corev1.EnvVar{Name: "MUSTER_REPO_BRANCH", Value: spec.Repo.Branch})
		}
		if p := spec.Repo.Path; p != "" {
			env = append(env, corev1.EnvVar{Name: "MUSTER_REPO_PATH", Value: p})
		}
	}
	// Config is opaque: it is handed over verbatim as JSON and no key of it is
	// interpreted here.
	if len(spec.Config) > 0 {
		b, err := json.Marshal(spec.Config)
		if err != nil {
			return nil, fmt.Errorf("%w: Spec.Config is not JSON-serialisable: %v", provision.ErrInvalidSpec, err)
		}
		env = append(env, corev1.EnvVar{Name: "MUSTER_CONFIG", Value: string(b)})
	}

	resources, err := renderResources(spec.Resources)
	if err != nil {
		return nil, err
	}

	volumes := d.renderVolumes(spec)

	wsPath := spec.Workspace.Path
	if wsPath == "" {
		wsPath = DefaultWorkspacePath
	}
	// The instance's container mounts ONLY its own files (a sidecar's are in
	// the same objects; renderSidecar mounts those).
	mounts := fileMounts(spec.Files)
	mounts = append(mounts, corev1.VolumeMount{Name: workspaceVolumeName, MountPath: wsPath})

	ports := make([]corev1.ContainerPort, 0, len(spec.Ports))
	for _, p := range spec.Ports {
		proto := corev1.ProtocolTCP
		if strings.EqualFold(p.Protocol, "UDP") {
			proto = corev1.ProtocolUDP
		}
		name := p.Name
		if name == "" {
			name = "port-" + strconv.Itoa(p.Port)
		}
		ports = append(ports, corev1.ContainerPort{Name: name, ContainerPort: int32(p.Port), Protocol: proto})
	}

	container := corev1.Container{
		Name:         containerName,
		Image:        spec.Runtime.Image,
		Command:      spec.Runtime.Command,
		Args:         spec.Runtime.Args,
		WorkingDir:   spec.Runtime.WorkingDir,
		Env:          env,
		Ports:        ports,
		Resources:    resources,
		VolumeMounts: mounts,
		// ⚠ ON THIS CONTAINER ONLY, NEVER ON THE INIT CONTAINER. An init
		// container runs to completion and is not supervised; a probe on one is
		// ignored by the apiserver for a startup/liveness pair, so attaching them
		// there would read as coverage and provide none. renderInitContainer
		// copies Env, EnvFrom and VolumeMounts off this value deliberately and
		// these two fields are deliberately not among them.
		StartupProbe:  startupProbe(spec),
		LivenessProbe: livenessProbe(spec),
	}
	if secret := d.renderEnvSecret(spec, ns); secret != nil {
		// envFrom, not one EnvVar per secret key: the values must not appear in
		// the pod spec, which is readable by anything that can read the
		// Deployment.
		//
		// 🔴 IT IS THE ENV SECRET, AND ONLY THE ENV SECRET. envFrom exports
		// every key of the referenced object as an environment variable, so
		// naming the object that holds secret FILE content here would put those
		// files in the process environment. A spec with confidential files and
		// no confidential environment therefore gets NO envFrom at all, which
		// is what renderEnvSecret returning nil expresses.
		container.EnvFrom = append(container.EnvFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: secret.Name},
			},
		})
	}
	container.SecurityContext = containerSecurity(spec.Security)
	containers := []corev1.Container{container}
	if spec.Sidecar != nil {
		sc, err := d.renderSidecar(spec, ns)
		if err != nil {
			return nil, err
		}
		containers = append(containers, sc)
	}
	podSpec := corev1.PodSpec{
		ServiceAccountName: spec.Ref.Name,
		Containers:         containers,
		Volumes:            volumes,
		SecurityContext:    podSecurity(spec.Security),
	}
	if spec.Security.NoServiceAccountToken {
		no := false
		podSpec.AutomountServiceAccountToken = &no
	}
	if len(spec.Init) > 0 {
		podSpec.InitContainers = []corev1.Container{d.renderInitContainer(spec, container)}
	}

	replicas := int32(spec.DesiredReplicas())
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        spec.Ref.Name,
			Namespace:   ns,
			Labels:      labels,
			Annotations: d.renderAnnotations(spec),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: instanceLabels(spec.Ref.Name)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{annFingerprint: provision.Fingerprint(spec)},
				},
				Spec: podSpec,
			},
			Strategy: deploymentStrategy(spec),
		},
	}
	return dep, nil
}

// deploymentStrategy picks Recreate for a persistent workspace and
// RollingUpdate otherwise.
//
// 🔴 THE PERSISTENT CASE IS NOT A PREFERENCE. A ReadWriteOnce volume can be
// attached to one node at a time, so a rolling update deadlocks: the new pod
// waits for a volume the old pod will not release until the new pod is ready.
// The symptom is a Deployment stuck in Pending with a scheduling event about
// volume attachment, which reads as a cluster problem rather than a strategy
// one.
func deploymentStrategy(spec provision.Spec) appsv1.DeploymentStrategy {
	if spec.Workspace.Persist {
		return appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	}
	maxUnavailable := intstr.FromInt32(0)
	maxSurge := intstr.FromInt32(1)
	return appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxUnavailable: &maxUnavailable,
			MaxSurge:       &maxSurge,
		},
	}
}

func (d *Driver) renderAnnotations(spec provision.Spec) map[string]string {
	ann := map[string]string{
		annFingerprint: provision.Fingerprint(spec),
	}
	if spec.Ref.ID != 0 {
		ann[annCorrelationID] = strconv.FormatInt(spec.Ref.ID, 10)
	}
	if spec.Endpoint != nil && !spec.Endpoint.IsZero() {
		if b, err := json.Marshal(spec.Endpoint); err == nil {
			ann[annEndpoint] = string(b)
		}
	}
	if port := spec.PortNumber(provision.DefaultPortName); port != 0 {
		ann[AnnotationPort] = strconv.Itoa(port)
	}
	return ann
}

// podSecurity and containerSecurity translate [provision.Security].
//
// 🔴 BOTH RETURN nil FOR A ZERO Security, AND THAT IS WHAT KEEPS EVERY
// PRE-EXISTING SPEC'S POD TEMPLATE BYTE-IDENTICAL. An empty-but-non-nil
// SecurityContext is a different object to the apiserver and to a golden file;
// a spec that declares nothing must render exactly what it rendered before the
// type existed.
func podSecurity(sec provision.Security) *corev1.PodSecurityContext {
	if sec.RunAsUser == 0 && sec.RunAsGroup == 0 && sec.FSGroup == 0 && !sec.RunAsNonRoot && !sec.Restricted {
		return nil
	}
	psc := &corev1.PodSecurityContext{}
	if sec.RunAsUser != 0 {
		v := sec.RunAsUser
		psc.RunAsUser = &v
	}
	if sec.RunAsGroup != 0 {
		v := sec.RunAsGroup
		psc.RunAsGroup = &v
	}
	if sec.FSGroup != 0 {
		v := sec.FSGroup
		psc.FSGroup = &v
	}
	if sec.RunAsNonRoot {
		t := true
		psc.RunAsNonRoot = &t
	}
	if sec.Restricted {
		psc.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	}
	return psc
}

func containerSecurity(sec provision.Security) *corev1.SecurityContext {
	if !sec.Restricted {
		return nil
	}
	no := false
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &no,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// renderInitContainer runs Spec.Init before the application container.
//
// 🔴 IT RUNS UNDER `sh -eu` AND DOES NOT SWALLOW FAILURES, WHICH IS THE
// OPPOSITE OF WHAT IT REPLACES. The project this came from appended file
// placement to the same list of shell commands and then wrapped the whole block
// in `{ … } || :` so a failure could not abort startup — necessary there,
// because a missing convenience file would otherwise have been an outage, and
// dangerous because it meant nothing in that block could ever be observed to
// fail. Here, content travels as Spec.Files and is placed by the kubelet, so
// Init holds only steps that are genuinely imperative and genuinely required.
// A failing one SHOULD stop the pod: that is a crash loop with a log, rather
// than a running agent missing something it needs.
func (d *Driver) renderInitContainer(spec provision.Spec, main corev1.Container) corev1.Container {
	wsPath := spec.Workspace.Path
	if wsPath == "" {
		wsPath = DefaultWorkspacePath
	}
	return corev1.Container{
		Name:  initContainerName,
		Image: spec.Runtime.Image,
		// -e: stop at the first failure. -u: an unset variable is an error, not
		// an empty string spliced into a path.
		Command:      []string{"sh", "-eu", "-c", strings.Join(spec.Init, "\n")},
		Env:          main.Env,
		EnvFrom:      main.EnvFrom,
		VolumeMounts: main.VolumeMounts,
		WorkingDir:   wsPath,
	}
}

// renderSidecar is the sidecar's container (provision.Sidecar): its own image,
// command, environment, confidential environment (its own Secret) and files,
// and the instance's container security. NOT the workspace volume (see the
// type). No ports and no probes: it is reached on loopback by the instance's
// process, which is what the pod's probes check.
//
// ⚠ NO PROBE IS DELIBERATE, NOT MISSING: every container's readiness gates the
// pod's, so a probe here could only add ways for a sidecar fault to drop the
// INSTANCE's endpoint. A sidecar that must survive its own process's exit says
// so in its command (the removed gateway sidecar ran a supervisor); the kubelet
// restarting an exited sidecar takes the pod NotReady while it does.
func (d *Driver) renderSidecar(spec provision.Spec, ns string) (corev1.Container, error) {
	sc := spec.Sidecar
	if sc.Name == containerName || sc.Name == initContainerName {
		return corev1.Container{}, fmt.Errorf("%w: sidecar name %q is the instance's own container's", provision.ErrInvalidSpec, sc.Name)
	}
	resources, err := renderResources(sc.Resources)
	if err != nil {
		return corev1.Container{}, err
	}
	env := make([]corev1.EnvVar, 0, len(sc.Env))
	for _, e := range sc.Env {
		env = append(env, corev1.EnvVar{Name: e.Name, Value: e.Value})
	}
	mounts := fileMounts(sc.Files)
	c := corev1.Container{
		Name:            sc.Name,
		Image:           sc.Runtime.Image,
		Command:         sc.Runtime.Command,
		Args:            sc.Runtime.Args,
		WorkingDir:      sc.Runtime.WorkingDir,
		Env:             env,
		Resources:       resources,
		VolumeMounts:    mounts,
		SecurityContext: containerSecurity(spec.Security),
	}
	if secret := d.renderSidecarEnvSecret(spec, ns); secret != nil {
		c.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: secret.Name}}}}
	}
	return c, nil
}

// fileMounts mounts files from the two file volumes (renderVolumes), each at
// its path through a subPath of its key.
func fileMounts(files []provision.File) []corev1.VolumeMount {
	plain, secretFiles := sortedFiles(files)
	var mounts []corev1.VolumeMount
	for _, f := range plain {
		mounts = append(mounts, corev1.VolumeMount{Name: filesVolumeName, MountPath: f.Path, SubPath: fileKey(f.Path), ReadOnly: true})
	}
	for _, f := range secretFiles {
		mounts = append(mounts, corev1.VolumeMount{Name: secretFilesVolumeName, MountPath: f.Path, SubPath: fileKey(f.Path), ReadOnly: true})
	}
	return mounts
}

// renderVolumes projects the pod's files — the instance's and its sidecar's —
// and its workspace. Each container mounts its own files from these
// (fileMounts): one volumeMount per file with a subPath, which is what places a
// single key at an arbitrary absolute path rather than turning its directory
// into a mount point. Per-file modes travel on the volume's items.
func (d *Driver) renderVolumes(spec provision.Spec) []corev1.Volume {
	plain, secretFiles := sortedFiles(allFiles(spec))

	var volumes []corev1.Volume

	if len(plain) > 0 {
		items := make([]corev1.KeyToPath, 0, len(plain))
		for _, f := range plain {
			mode := int32(f.EffectiveMode())
			key := fileKey(f.Path)
			items = append(items, corev1.KeyToPath{Key: key, Path: key, Mode: &mode})
		}
		volumes = append(volumes, corev1.Volume{
			Name: filesVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: configMapName(spec.Ref.Name)},
					Items:                items,
				},
			},
		})
	}

	if len(secretFiles) > 0 {
		items := make([]corev1.KeyToPath, 0, len(secretFiles))
		for _, f := range secretFiles {
			mode := int32(f.EffectiveMode())
			key := fileKey(f.Path)
			items = append(items, corev1.KeyToPath{Key: key, Path: key, Mode: &mode})
		}
		volumes = append(volumes, corev1.Volume{
			Name: secretFilesVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: fileSecretName(spec.Ref.Name),
					Items:      items,
				},
			},
		})
	}

	ws := corev1.Volume{Name: workspaceVolumeName}
	if spec.Workspace.Persist {
		ws.VolumeSource = corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: pvcName(spec.Ref.Name),
			},
		}
	} else {
		ws.VolumeSource = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	}
	volumes = append(volumes, ws)

	return volumes
}

func renderResources(r provision.Resources) (corev1.ResourceRequirements, error) {
	out := corev1.ResourceRequirements{}
	set := func(list *corev1.ResourceList, name corev1.ResourceName, value, field string) error {
		if value == "" {
			return nil
		}
		q, err := resource.ParseQuantity(value)
		if err != nil {
			return fmt.Errorf("%w: resources.%s = %q: %v", provision.ErrInvalidSpec, field, value, err)
		}
		if *list == nil {
			*list = corev1.ResourceList{}
		}
		(*list)[name] = q
		return nil
	}
	if err := set(&out.Requests, corev1.ResourceCPU, r.CPURequest, "CPURequest"); err != nil {
		return out, err
	}
	if err := set(&out.Requests, corev1.ResourceMemory, r.MemoryRequest, "MemoryRequest"); err != nil {
		return out, err
	}
	if err := set(&out.Limits, corev1.ResourceCPU, r.CPULimit, "CPULimit"); err != nil {
		return out, err
	}
	if err := set(&out.Limits, corev1.ResourceMemory, r.MemoryLimit, "MemoryLimit"); err != nil {
		return out, err
	}
	return out, nil
}
