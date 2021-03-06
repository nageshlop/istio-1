// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package inject

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ghodss/yaml"
	kubeApiAdmissionv1 "k8s.io/api/admission/v1"
	kubeApiAdmissionv1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	kjson "k8s.io/apimachinery/pkg/runtime/serializer/json"

	"istio.io/api/annotation"
	"istio.io/api/label"
	meshconfig "istio.io/api/mesh/v1alpha1"
	"istio.io/istio/pilot/cmd/pilot-agent/status"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/kube"
	"istio.io/pkg/log"
)

const proxyUIDAnnotation = "sidecar.istio.io/proxyUID"

var (
	runtimeScheme     = runtime.NewScheme()
	codecs            = serializer.NewCodecFactory(runtimeScheme)
	deserializer      = codecs.UniversalDeserializer()
	jsonSerializer    = kjson.NewSerializerWithOptions(kjson.DefaultMetaFactory, runtimeScheme, runtimeScheme, kjson.SerializerOptions{})
	URLParameterToEnv = map[string]string{
		"cluster": "ISTIO_META_CLUSTER_ID",
		"net":     "ISTIO_META_NETWORK",
	}
)

func init() {
	_ = corev1.AddToScheme(runtimeScheme)
	_ = kubeApiAdmissionv1.AddToScheme(runtimeScheme)
	_ = kubeApiAdmissionv1beta1.AddToScheme(runtimeScheme)
}

const (
	watchDebounceDelay = 100 * time.Millisecond
)

// Webhook implements a mutating webhook for automatic proxy injection.
type Webhook struct {
	mu                     sync.RWMutex
	Config                 *Config
	sidecarTemplateVersion string
	meshConfig             *meshconfig.MeshConfig
	valuesConfig           string

	healthCheckInterval time.Duration
	healthCheckFile     string

	watcher Watcher

	mon      *monitor
	env      *model.Environment
	revision string
}

//nolint directives: interfacer
func loadConfig(injectFile, valuesFile string) (*Config, string, error) {
	data, err := ioutil.ReadFile(injectFile)
	if err != nil {
		return nil, "", err
	}
	var c *Config
	if c, err = unmarshalConfig(data); err != nil {
		log.Warnf("Failed to parse injectFile %s", string(data))
		return nil, "", err
	}

	valuesConfig, err := ioutil.ReadFile(valuesFile)
	if err != nil {
		return nil, "", err
	}
	return c, string(valuesConfig), nil
}

func unmarshalConfig(data []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, err
	}

	log.Debugf("New inject configuration: sha256sum %x", sha256.Sum256(data))
	log.Debugf("Policy: %v", c.Policy)
	log.Debugf("AlwaysInjectSelector: %v", c.AlwaysInjectSelector)
	log.Debugf("NeverInjectSelector: %v", c.NeverInjectSelector)
	log.Debugf("Template: |\n  %v", strings.Replace(c.Template, "\n", "\n  ", -1))
	return &c, nil
}

// WebhookParameters configures parameters for the sidecar injection
// webhook.
type WebhookParameters struct {
	// Watcher watches the sidecar injection configuration.
	Watcher Watcher

	// Port is the webhook port, e.g. typically 443 for https.
	// This is mainly used for tests. Webhook runs on the port started by Istiod.
	Port int

	// MonitoringPort is the webhook port, e.g. typically 15014.
	// Set to -1 to disable monitoring
	MonitoringPort int

	// HealthCheckInterval configures how frequently the health check
	// file is updated. Value of zero disables the health check
	// update.
	HealthCheckInterval time.Duration

	// HealthCheckFile specifies the path to the health check file
	// that is periodically updated.
	HealthCheckFile string

	Env *model.Environment

	// Use an existing mux instead of creating our own.
	Mux *http.ServeMux

	// The istio.io/rev this injector is responsible for
	Revision string
}

// NewWebhook creates a new instance of a mutating webhook for automatic sidecar injection.
func NewWebhook(p WebhookParameters) (*Webhook, error) {
	if p.Mux == nil {
		return nil, errors.New("expected mux to be passed, but was not passed")
	}

	wh := &Webhook{
		watcher:             p.Watcher,
		meshConfig:          p.Env.Mesh(),
		healthCheckInterval: p.HealthCheckInterval,
		healthCheckFile:     p.HealthCheckFile,
		env:                 p.Env,
		revision:            p.Revision,
	}

	p.Watcher.SetHandler(wh.updateConfig)
	sidecarConfig, valuesConfig, err := p.Watcher.Get()
	if err != nil {
		return nil, err
	}
	wh.updateConfig(sidecarConfig, valuesConfig)

	p.Mux.HandleFunc("/inject", wh.serveInject)
	p.Mux.HandleFunc("/inject/", wh.serveInject)

	p.Env.Watcher.AddMeshHandler(func() {
		wh.mu.Lock()
		wh.meshConfig = p.Env.Mesh()
		wh.mu.Unlock()
	})

	if p.MonitoringPort >= 0 {
		mon, err := startMonitor(p.Mux, p.MonitoringPort)
		if err != nil {
			return nil, fmt.Errorf("could not start monitoring server %v", err)
		}
		wh.mon = mon
	}

	return wh, nil
}

// Run implements the webhook server
func (wh *Webhook) Run(stop <-chan struct{}) {
	go wh.watcher.Run(stop)

	if wh.mon != nil {
		defer wh.mon.monitoringServer.Close()
	}

	var healthC <-chan time.Time
	if wh.healthCheckInterval != 0 && wh.healthCheckFile != "" {
		t := time.NewTicker(wh.healthCheckInterval)
		healthC = t.C
		defer t.Stop()
	}

	for {
		select {
		case <-healthC:
			content := []byte(`ok`)
			if err := ioutil.WriteFile(wh.healthCheckFile, content, 0644); err != nil {
				log.Errorf("Health check update of %q failed: %v", wh.healthCheckFile, err)
			}
		case <-stop:
			return
		}
	}
}

func (wh *Webhook) updateConfig(sidecarConfig *Config, valuesConfig string) {
	version := sidecarTemplateVersionHash(sidecarConfig.Template)
	wh.mu.Lock()
	wh.Config = sidecarConfig
	wh.valuesConfig = valuesConfig
	wh.sidecarTemplateVersion = version
	wh.mu.Unlock()
}

// It would be great to use https://github.com/mattbaird/jsonpatch to
// generate RFC6902 JSON patches. Unfortunately, it doesn't produce
// correct patches for object removal. Fortunately, our patching needs
// are fairly simple so generating them manually isn't horrible (yet).
type rfc6902PatchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// JSONPatch `remove` is applied sequentially. Remove items in reverse
// order to avoid renumbering indices.
func removeContainers(containers []corev1.Container, removed []string, path string) (patch []rfc6902PatchOperation) {
	names := map[string]bool{}
	for _, name := range removed {
		names[name] = true
	}
	for i := len(containers) - 1; i >= 0; i-- {
		if _, ok := names[containers[i].Name]; ok {
			patch = append(patch, rfc6902PatchOperation{
				Op:   "remove",
				Path: fmt.Sprintf("%v/%v", path, i),
			})
		}
	}
	return patch
}

func removeVolumes(volumes []corev1.Volume, removed []string, path string) (patch []rfc6902PatchOperation) {
	names := map[string]bool{}
	for _, name := range removed {
		names[name] = true
	}
	for i := len(volumes) - 1; i >= 0; i-- {
		if _, ok := names[volumes[i].Name]; ok {
			patch = append(patch, rfc6902PatchOperation{
				Op:   "remove",
				Path: fmt.Sprintf("%v/%v", path, i),
			})
		}
	}
	return patch
}

func removeImagePullSecrets(imagePullSecrets []corev1.LocalObjectReference, removed []string, path string) (patch []rfc6902PatchOperation) {
	names := map[string]bool{}
	for _, name := range removed {
		names[name] = true
	}
	for i := len(imagePullSecrets) - 1; i >= 0; i-- {
		if _, ok := names[imagePullSecrets[i].Name]; ok {
			patch = append(patch, rfc6902PatchOperation{
				Op:   "remove",
				Path: fmt.Sprintf("%v/%v", path, i),
			})
		}
	}
	return patch
}

func addContainer(sic *SidecarInjectionSpec, target, added []corev1.Container, basePath string) (patch []rfc6902PatchOperation) {
	saJwtSecretMountName := ""
	var saJwtSecretMount corev1.VolumeMount
	// find service account secret volume mount(/var/run/secrets/kubernetes.io/serviceaccount,
	// https://kubernetes.io/docs/reference/access-authn-authz/service-accounts-admin/#service-account-automation) from app container
	for _, add := range target {
		for _, vmount := range add.VolumeMounts {
			if vmount.MountPath == "/var/run/secrets/kubernetes.io/serviceaccount" {
				saJwtSecretMountName = vmount.Name
				saJwtSecretMount = vmount
			}
		}
	}
	first := len(target) == 0
	var value interface{}
	for _, add := range added {
		if add.Name == sidecarContainerName && saJwtSecretMountName != "" {
			// add service account secret volume mount(/var/run/secrets/kubernetes.io/serviceaccount,
			// https://kubernetes.io/docs/reference/access-authn-authz/service-accounts-admin/#service-account-automation) to istio-proxy container,
			// so that envoy could fetch/pass k8s sa jwt and pass to sds server, which will be used to request workload identity for the pod.
			add.VolumeMounts = append(add.VolumeMounts, saJwtSecretMount)
		}
		value = add
		path := basePath
		if first {
			first = false
			value = []corev1.Container{add}
		} else if shouldBeInjectedInFront(add, sic) {
			path += "/0"
		} else {
			path += "/-"
		}
		patch = append(patch, rfc6902PatchOperation{
			Op:    "add",
			Path:  path,
			Value: value,
		})
	}
	return patch
}

func shouldBeInjectedInFront(container corev1.Container, sic *SidecarInjectionSpec) bool {
	switch container.Name {
	case ValidationContainerName:
		return true
	case ProxyContainerName:
		return sic.HoldApplicationUntilProxyStarts
	default:
		return false
	}
}

func addSecurityContext(target *corev1.PodSecurityContext, basePath string) (patch []rfc6902PatchOperation) {
	patch = append(patch, rfc6902PatchOperation{
		Op:    "add",
		Path:  basePath,
		Value: target,
	})
	return patch
}

func addVolume(target, added []corev1.Volume, basePath string) (patch []rfc6902PatchOperation) {
	first := len(target) == 0
	var value interface{}
	for _, add := range added {
		value = add
		path := basePath
		if first {
			first = false
			value = []corev1.Volume{add}
		} else {
			path += "/-"
		}
		patch = append(patch, rfc6902PatchOperation{
			Op:    "add",
			Path:  path,
			Value: value,
		})
	}
	return patch
}

func addImagePullSecrets(target, added []corev1.LocalObjectReference, basePath string) (patch []rfc6902PatchOperation) {
	first := len(target) == 0
	var value interface{}
	for _, add := range added {
		value = add
		path := basePath
		if first {
			first = false
			value = []corev1.LocalObjectReference{add}
		} else {
			path += "/-"
		}
		patch = append(patch, rfc6902PatchOperation{
			Op:    "add",
			Path:  path,
			Value: value,
		})
	}
	return patch
}

func addPodDNSConfig(target *corev1.PodDNSConfig, basePath string) (patch []rfc6902PatchOperation) {
	patch = append(patch, rfc6902PatchOperation{
		Op:    "add",
		Path:  basePath,
		Value: target,
	})
	return patch
}

// escape JSON Pointer value per https://tools.ietf.org/html/rfc6901
func escapeJSONPointerValue(in string) string {
	step := strings.Replace(in, "~", "~0", -1)
	return strings.Replace(step, "/", "~1", -1)
}

// adds labels to the target spec, will not overwrite label's value if it already exists
func addLabels(target map[string]string, added map[string]string) []rfc6902PatchOperation {
	patches := []rfc6902PatchOperation{}

	addedKeys := make([]string, 0, len(added))
	for key := range added {
		addedKeys = append(addedKeys, key)
	}
	sort.Strings(addedKeys)

	for _, key := range addedKeys {
		value := added[key]
		patch := rfc6902PatchOperation{
			Op:    "add",
			Path:  "/metadata/labels/" + escapeJSONPointerValue(key),
			Value: value,
		}

		if target == nil {
			target = map[string]string{}
			patch.Path = "/metadata/labels"
			patch.Value = map[string]string{
				key: value,
			}
		}

		if target[key] == "" {
			patches = append(patches, patch)
		}
	}

	return patches
}

func updateAnnotation(target map[string]string, added map[string]string) (patch []rfc6902PatchOperation) {
	// To ensure deterministic patches, we sort the keys
	var keys []string
	for k := range added {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		value := added[key]
		if target == nil {
			target = map[string]string{}
			patch = append(patch, rfc6902PatchOperation{
				Op:   "add",
				Path: "/metadata/annotations",
				Value: map[string]string{
					key: value,
				},
			})
		} else {
			op := "add"
			if target[key] != "" {
				op = "replace"
			}
			patch = append(patch, rfc6902PatchOperation{
				Op:    op,
				Path:  "/metadata/annotations/" + escapeJSONPointerValue(key),
				Value: value,
			})
		}
	}
	return patch
}

func createPatch(pod *corev1.Pod, prevStatus *SidecarInjectionStatus, revision string, annotations map[string]string,
	sic *SidecarInjectionSpec, workloadName string, mesh *meshconfig.MeshConfig) ([]byte, error) {

	var patch []rfc6902PatchOperation

	rewrite := ShouldRewriteAppHTTPProbers(pod.Annotations, sic)

	sidecar := FindSidecar(sic.Containers)
	// We don't have to escape json encoding here when using golang libraries.
	if rewrite && sidecar != nil {
		if prober := DumpAppProbers(&pod.Spec); prober != "" {
			sidecar.Env = append(sidecar.Env, corev1.EnvVar{Name: status.KubeAppProberEnvName, Value: prober})
		}
	}

	if rewrite {
		patch = append(patch, createProbeRewritePatch(pod.Annotations, &pod.Spec, sic, mesh.GetDefaultConfig().GetStatusPort())...)
	}

	// Remove any containers previously injected by kube-inject using
	// container and volume name as unique key for removal.
	patch = append(patch, removeContainers(pod.Spec.InitContainers, prevStatus.InitContainers, "/spec/initContainers")...)
	patch = append(patch, removeContainers(pod.Spec.Containers, prevStatus.Containers, "/spec/containers")...)
	patch = append(patch, removeVolumes(pod.Spec.Volumes, prevStatus.Volumes, "/spec/volumes")...)
	patch = append(patch, removeImagePullSecrets(pod.Spec.ImagePullSecrets, prevStatus.ImagePullSecrets, "/spec/imagePullSecrets")...)

	if enablePrometheusMerge(mesh, pod.ObjectMeta.Annotations) {
		scrape := status.PrometheusScrapeConfiguration{
			Scrape: pod.ObjectMeta.Annotations["prometheus.io/scrape"],
			Path:   pod.ObjectMeta.Annotations["prometheus.io/path"],
			Port:   pod.ObjectMeta.Annotations["prometheus.io/port"],
		}
		empty := status.PrometheusScrapeConfiguration{}
		if sidecar != nil && scrape != empty {
			by, err := json.Marshal(scrape)
			if err != nil {
				return nil, err
			}
			sidecar.Env = append(sidecar.Env, corev1.EnvVar{Name: status.PrometheusScrapingConfig.Name, Value: string(by)})
		}
		annotations["prometheus.io/port"] = strconv.Itoa(int(mesh.GetDefaultConfig().GetStatusPort()))
		annotations["prometheus.io/path"] = "/stats/prometheus"
		annotations["prometheus.io/scrape"] = "true"
	}

	patch = append(patch, addContainer(sic, pod.Spec.InitContainers, sic.InitContainers, "/spec/initContainers")...)
	patch = append(patch, addContainer(sic, pod.Spec.Containers, sic.Containers, "/spec/containers")...)
	patch = append(patch, addVolume(pod.Spec.Volumes, sic.Volumes, "/spec/volumes")...)
	patch = append(patch, addImagePullSecrets(pod.Spec.ImagePullSecrets, sic.ImagePullSecrets, "/spec/imagePullSecrets")...)

	if sic.DNSConfig != nil {
		patch = append(patch, addPodDNSConfig(sic.DNSConfig, "/spec/dnsConfig")...)
	}

	if pod.Spec.SecurityContext != nil {
		patch = append(patch, addSecurityContext(pod.Spec.SecurityContext, "/spec/securityContext")...)
	}

	patch = append(patch, updateAnnotation(pod.Annotations, annotations)...)

	canonicalSvc, canonicalRev := ExtractCanonicalServiceLabels(pod.Labels, workloadName)
	patchLabels := map[string]string{
		label.TLSMode:                                model.IstioMutualTLSModeLabel,
		model.IstioCanonicalServiceLabelName:         canonicalSvc,
		label.IstioRev:                               revision,
		model.IstioCanonicalServiceRevisionLabelName: canonicalRev,
	}
	if network := topologyValues(sic); network != "" {
		// only added if if not already set
		patchLabels[label.IstioNetwork] = network
	}
	patch = append(patch, addLabels(pod.Labels, patchLabels)...)

	return json.Marshal(patch)
}

// topologyValues will find the value of ISTIO_META_NETWORK in the spec or return a zero-value
func topologyValues(sic *SidecarInjectionSpec) string {
	// TODO should we just return the values used to populate the template from InjectionData?
	for _, c := range sic.Containers {
		for _, e := range c.Env {
			if e.Name == "ISTIO_META_NETWORK" {
				return e.Value
			}
		}
	}
	return ""
}

func enablePrometheusMerge(mesh *meshconfig.MeshConfig, anno map[string]string) bool {
	// If annotation is present, we look there first
	if val, f := anno[annotation.PrometheusMergeMetrics.Name]; f {
		bval, err := strconv.ParseBool(val)
		if err != nil {
			// This shouldn't happen since we validate earlier in the code
			log.Warnf("invalid annotation %v=%v", annotation.PrometheusMergeMetrics.Name, bval)
		} else {
			return bval
		}
	}
	// If mesh config setting is present, use that
	if mesh.GetEnablePrometheusMerge() != nil {
		return mesh.GetEnablePrometheusMerge().Value
	}
	// Otherwise, we default to enable
	return true
}

func ExtractCanonicalServiceLabels(podLabels map[string]string, workloadName string) (string, string) {
	return extractCanonicalServiceLabel(podLabels, workloadName), extractCanonicalServiceRevision(podLabels)
}

func extractCanonicalServiceRevision(podLabels map[string]string) string {
	if rev, ok := podLabels[model.IstioCanonicalServiceRevisionLabelName]; ok {
		return rev
	}

	if rev, ok := podLabels["app.kubernetes.io/version"]; ok {
		return rev
	}

	if rev, ok := podLabels["version"]; ok {
		return rev
	}

	return "latest"
}

func extractCanonicalServiceLabel(podLabels map[string]string, workloadName string) string {
	if svc, ok := podLabels[model.IstioCanonicalServiceLabelName]; ok {
		return svc
	}

	if svc, ok := podLabels["app.kubernetes.io/name"]; ok {
		return svc
	}

	if svc, ok := podLabels["app"]; ok {
		return svc
	}

	return workloadName
}

// Retain deprecated hardcoded container and volumes names to aid in
// backwards compatible migration to the new SidecarInjectionStatus.
var (
	initContainerName    = "istio-init"
	sidecarContainerName = "istio-proxy"

	legacyInitContainerNames = []string{initContainerName, "enable-core-dump"}
	legacyContainerNames     = []string{sidecarContainerName}
	legacyVolumeNames        = []string{"istio-certs", "istio-envoy"}
)

func injectionStatus(pod *corev1.Pod) *SidecarInjectionStatus {
	var statusBytes []byte
	if pod.ObjectMeta.Annotations != nil {
		if value, ok := pod.ObjectMeta.Annotations[annotation.SidecarStatus.Name]; ok {
			statusBytes = []byte(value)
		}
	}

	// default case when injected pod has explicit status
	var iStatus SidecarInjectionStatus
	if err := json.Unmarshal(statusBytes, &iStatus); err == nil {
		// heuristic assumes status is valid if any of the resource
		// lists is non-empty.
		if len(iStatus.InitContainers) != 0 ||
			len(iStatus.Containers) != 0 ||
			len(iStatus.Volumes) != 0 ||
			len(iStatus.ImagePullSecrets) != 0 {
			return &iStatus
		}
	}

	// backwards compatibility case when injected pod has legacy
	// status. Infer status from the list of legacy hardcoded
	// container and volume names.
	return &SidecarInjectionStatus{
		InitContainers: legacyInitContainerNames,
		Containers:     legacyContainerNames,
		Volumes:        legacyVolumeNames,
	}
}

func toAdmissionResponse(err error) *kube.AdmissionResponse {
	return &kube.AdmissionResponse{Result: &metav1.Status{Message: err.Error()}}
}

type InjectionParameters struct {
	pod                 *corev1.Pod
	deployMeta          *metav1.ObjectMeta
	typeMeta            *metav1.TypeMeta
	template            string
	version             string
	meshConfig          *meshconfig.MeshConfig
	valuesConfig        string
	revision            string
	proxyEnvs           map[string]string
	injectedAnnotations map[string]string
	proxyUID            uint64
	proxyGID            *int64
}

func injectPod(req InjectionParameters, partialInjection bool) ([]byte, error) {
	pod := req.pod

	if features.EnableLegacyFSGroupInjection {
		// due to bug https://github.com/kubernetes/kubernetes/issues/57923,
		// k8s sa jwt token volume mount file is only accessible to root user, not istio-proxy(the user that istio proxy runs as).
		// workaround by https://kubernetes.io/docs/tasks/configure-pod-container/security-context/#set-the-security-context-for-a-pod
		var grp = int64(1337)
		if req.proxyGID != nil {
			grp = *req.proxyGID
		}
		if pod.Spec.SecurityContext == nil {
			pod.Spec.SecurityContext = &corev1.PodSecurityContext{
				FSGroup: &grp,
			}
		} else {
			pod.Spec.SecurityContext.FSGroup = &grp
		}
	}

	spec, iStatus, err := InjectionData(req, req.typeMeta, req.deployMeta)
	if err != nil {
		return nil, err
	}

	annotations := map[string]string{annotation.SidecarStatus.Name: iStatus}

	// Add all additional injected annotations
	for k, v := range req.injectedAnnotations {
		annotations[k] = v
	}

	var patchBytes []byte
	if partialInjection {
		patchBytes, err = createPartialPatch(pod, req.injectedAnnotations, req.proxyUID)
		if err != nil {
			return nil, err
		}
	} else {
		replaceProxyRunAsUserID(spec, req.proxyUID)
		patchBytes, err = createPatch(pod, injectionStatus(pod), req.revision, annotations, spec, req.deployMeta.Name, req.meshConfig)
		if err != nil {
			return nil, err
		}
	}

	log.Debugf("AdmissionResponse: patch=%v\n", string(patchBytes))
	return patchBytes, nil
}

func (wh *Webhook) inject(ar *kube.AdmissionReview, path string) *kube.AdmissionResponse {
	req := ar.Request
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		handleError(fmt.Sprintf("Could not unmarshal raw object: %v %s", err,
			string(req.Object.Raw)))
		return toAdmissionResponse(err)
	}

	// Deal with potential empty fields, e.g., when the pod is created by a deployment
	podName := potentialPodName(&pod.ObjectMeta)
	if pod.ObjectMeta.Namespace == "" {
		pod.ObjectMeta.Namespace = req.Namespace
	}
	log.Infof("Sidecar injection request for %v/%v", req.Namespace, podName)
	log.Debugf("Object: %v", string(req.Object.Raw))
	log.Debugf("OldObject: %v", string(req.OldObject.Raw))

	partialInjection := false
	if !injectRequired(ignoredNamespaces, wh.Config, &pod.Spec, &pod.ObjectMeta) {
		if wasInjectedThroughIstioctl(&pod) {
			log.Infof("Performing partial injection into pre-injected pod %s/%s (injecting Multus annotation and runAsUser id)", pod.ObjectMeta.Namespace, podName)
			partialInjection = true
		} else {
			log.Infof("Skipping %s/%s due to policy check", pod.ObjectMeta.Namespace, podName)
			totalSkippedInjections.Increment()
			return &kube.AdmissionResponse{
				Allowed: true,
			}
		}
	}

	var proxyGID *int64
	proxyUID, err := getProxyUID(pod)
	if err != nil {
		log.Infof("Could not get proxyUID from annotation: %v", err)
	}
	if proxyUID == nil {
		if pod.Spec.SecurityContext != nil && pod.Spec.SecurityContext.RunAsUser != nil {
			uid := uint64(*pod.Spec.SecurityContext.RunAsUser) + 1
			proxyUID = &uid
			gid := *pod.Spec.SecurityContext.RunAsUser
			// valid GID for fsGroup defaults to first int in UID range in OCP's restricted SCC
			proxyGID = &gid
		}
		for _, c := range pod.Spec.Containers {
			if c.SecurityContext != nil && c.SecurityContext.RunAsUser != nil {
				uid := uint64(*c.SecurityContext.RunAsUser) + 1
				gid := *c.SecurityContext.RunAsUser
				if proxyUID == nil || uid > *proxyUID {
					proxyUID = &uid
				}
				if proxyGID == nil {
					proxyGID = &gid
				}
			}
		}
	}
	if proxyUID == nil {
		uid := DefaultSidecarProxyUID
		proxyUID = &uid
	}
	if proxyGID == nil {
		gid := int64(DefaultSidecarProxyUID)
		proxyGID = &gid
	}

	deploy, typeMeta := kube.GetDeployMetaFromPod(&pod)
	params := InjectionParameters{
		pod:                 &pod,
		deployMeta:          deploy,
		typeMeta:            typeMeta,
		template:            wh.Config.Template,
		version:             wh.sidecarTemplateVersion,
		meshConfig:          wh.meshConfig,
		valuesConfig:        wh.valuesConfig,
		revision:            wh.revision,
		injectedAnnotations: wh.Config.InjectedAnnotations,
		proxyEnvs:           parseInjectEnvs(path),
		proxyUID:            *proxyUID,
		proxyGID:            proxyGID,
	}

	patchBytes, err := injectPod(params, partialInjection)
	if err != nil {
		handleError(fmt.Sprintf("Pod injection failed: %v", err))
		return toAdmissionResponse(err)
	}

	reviewResponse := kube.AdmissionResponse{
		Allowed: true,
		Patch:   patchBytes,
		PatchType: func() *string {
			pt := "JSONPatch"
			return &pt
		}(),
	}
	totalSuccessfulInjections.Increment()
	return &reviewResponse
}

func wasInjectedThroughIstioctl(pod *corev1.Pod) bool {
	_, found := pod.Annotations[annotation.SidecarStatus.Name]
	return found
}

func replaceProxyRunAsUserID(spec *SidecarInjectionSpec, proxyUID uint64) {
	for i, c := range spec.InitContainers {
		if c.Name == initContainerName {
			for j, arg := range c.Args {
				if arg == "-u" {
					spec.InitContainers[i].Args[j+1] = strconv.FormatUint(proxyUID, 10)
					break
				}
			}
			break
		}
	}
	for i, c := range spec.Containers {
		if c.Name == sidecarContainerName {
			if c.SecurityContext == nil {
				securityContext := corev1.SecurityContext{}
				spec.Containers[i].SecurityContext = &securityContext
			}
			proxyUIDasInt64 := int64(proxyUID)
			spec.Containers[i].SecurityContext.RunAsUser = &proxyUIDasInt64
			break
		}
	}
}

func createPartialPatch(pod *corev1.Pod, annotations map[string]string, proxyUID uint64) ([]byte, error) {
	var patch []rfc6902PatchOperation
	patch = append(patch, patchProxyRunAsUserID(pod, proxyUID)...)
	patch = append(patch, updateAnnotation(pod.Annotations, annotations)...)
	return json.Marshal(patch)
}

func patchProxyRunAsUserID(pod *corev1.Pod, proxyUID uint64) (patch []rfc6902PatchOperation) {
	for i, c := range pod.Spec.InitContainers {
		if c.Name == initContainerName {
			for j, arg := range c.Args {
				if arg == "-u" {
					patch = append(patch, rfc6902PatchOperation{
						Op:    "replace",
						Path:  fmt.Sprintf("/spec/initContainers/%d/args/%d", i, j+1), // j+1 because the uid is the next argument (after -u)
						Value: strconv.FormatUint(proxyUID, 10),
					})
					break
				}
			}
			break
		}
	}

	for i, c := range pod.Spec.Containers {
		if c.Name == sidecarContainerName {
			if c.SecurityContext == nil {
				proxyUIDasInt64 := int64(proxyUID)
				securityContext := corev1.SecurityContext{
					RunAsUser: &proxyUIDasInt64,
				}
				patch = append(patch, rfc6902PatchOperation{
					Op:    "add",
					Path:  fmt.Sprintf("/spec/containers/%d/securityContext", i),
					Value: securityContext,
				})
			} else if c.SecurityContext.RunAsUser == nil {
				patch = append(patch, rfc6902PatchOperation{
					Op:    "add",
					Path:  fmt.Sprintf("/spec/containers/%d/securityContext/runAsUser", i),
					Value: proxyUID,
				})
			} else {
				patch = append(patch, rfc6902PatchOperation{
					Op:    "replace",
					Path:  fmt.Sprintf("/spec/containers/%d/securityContext/runAsUser", i),
					Value: proxyUID,
				})
			}
			break
		}
	}

	return patch
}

func getProxyUID(pod corev1.Pod) (*uint64, error) {
	if pod.Annotations != nil {
		if annotationValue, found := pod.Annotations[proxyUIDAnnotation]; found {
			proxyUID, err := strconv.ParseUint(annotationValue, 10, 64)
			if err != nil {
				return nil, err
			}
			return &proxyUID, nil
		}
	}
	return nil, nil
}

func (wh *Webhook) serveInject(w http.ResponseWriter, r *http.Request) {
	totalInjections.Increment()
	var body []byte
	if r.Body != nil {
		if data, err := ioutil.ReadAll(r.Body); err == nil {
			body = data
		}
	}
	if len(body) == 0 {
		handleError("no body found")
		http.Error(w, "no body found", http.StatusBadRequest)
		return
	}

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		handleError(fmt.Sprintf("contentType=%s, expect application/json", contentType))
		http.Error(w, "invalid Content-Type, want `application/json`", http.StatusUnsupportedMediaType)
		return
	}

	path := ""
	if r.URL != nil {
		path = r.URL.Path
	}

	var reviewResponse *kube.AdmissionResponse
	var obj runtime.Object
	var ar *kube.AdmissionReview
	if out, _, err := deserializer.Decode(body, nil, obj); err != nil {
		handleError(fmt.Sprintf("Could not decode body: %v", err))
		reviewResponse = toAdmissionResponse(err)
	} else {
		log.Debugf("AdmissionRequest for path=%s\n", path)
		ar, err = kube.AdmissionReviewKubeToAdapter(out)
		if err != nil {
			handleError(fmt.Sprintf("Could not decode object: %v", err))
		}
		reviewResponse = wh.inject(ar, path)
	}

	response := kube.AdmissionReview{}
	response.Response = reviewResponse
	var responseKube runtime.Object
	var apiVersion string
	if ar != nil {
		apiVersion = ar.APIVersion
		response.TypeMeta = ar.TypeMeta
		if response.Response != nil {
			if ar.Request != nil {
				response.Response.UID = ar.Request.UID
			}
		}
	}
	responseKube = kube.AdmissionReviewAdapterToKube(&response, apiVersion)
	resp, err := json.Marshal(responseKube)
	if err != nil {
		log.Errorf("Could not encode response: %v", err)
		http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
	}
	if _, err := w.Write(resp); err != nil {
		log.Errorf("Could not write response: %v", err)
		http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
	}
}

// parseInjectEnvs parse new envs from inject url path
// follow format: /inject/k1/v1/k2/v2, any kv order works
// eg. "/inject/cluster/cluster1", "/inject/net/network1/cluster/cluster1"
func parseInjectEnvs(path string) map[string]string {
	path = strings.TrimSuffix(path, "/")
	res := strings.Split(path, "/")
	newEnvs := make(map[string]string)

	for i := 2; i < len(res); i += 2 { // skip '/inject'
		k := res[i]
		if i == len(res)-1 { // ignore the last key without value
			log.Warnf("Odd number of inject env entries, ignore the last key %s\n", k)
			break
		}

		env, found := URLParameterToEnv[k]
		if !found {
			env = strings.ToUpper(k) // if not found, use the custom env directly
		}
		if env != "" {
			newEnvs[env] = res[i+1]
		}
	}

	return newEnvs
}

func handleError(message string) {
	log.Errorf(message)
	totalFailedInjections.Increment()
}
