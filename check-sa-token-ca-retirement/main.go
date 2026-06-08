package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/homedir"
)

const (
	appName                        = "check-sa-token-ca-retirement"
	defaultServiceAccount          = "default"
	defaultServiceAccountMountPath = "/var/run/secrets/kubernetes.io/serviceaccount"
	defaultCACertPath              = defaultServiceAccountMountPath + "/ca.crt"
	defaultTokenPath               = defaultServiceAccountMountPath + "/token"

	legacyTokenLastUsedAnnotation   = "kubernetes.io/legacy-token-last-used"
	legacyTokenInvalidAnnotation    = "kubernetes.io/legacy-token-invalid-since"
	legacyTokenServiceAccountKey    = corev1.ServiceAccountNameKey
	legacyTokenServiceAccountUIDKey = corev1.ServiceAccountUIDKey
)

type options struct {
	kubeconfig                 string
	contextName                string
	namespace                  string
	output                     string
	maxSamples                 int
	includePodBreakdown        bool
	skipPodExec                bool
	skipLegacySecretInspection bool
	saKeyCutoverText           string
	saKeyCutover               *time.Time
	newCAFile                  string
	requiredCAFingerprints     map[string]struct{}
	caCertPath                 string
	tokenPath                  string
	now                        time.Time
}

type scanResult struct {
	Items                          []applicationFinding       `json:"items"`
	UnreferencedLegacyTokenSecrets []legacyTokenSecretFinding `json:"unreferencedLegacyTokenSecrets,omitempty"`
	Warnings                       []string                   `json:"warnings,omitempty"`
}

type applicationFinding struct {
	Namespace                  string                     `json:"namespace"`
	OwnerKind                  string                     `json:"ownerKind"`
	OwnerName                  string                     `json:"ownerName"`
	ServiceAccounts            []string                   `json:"serviceAccounts"`
	IssueTypes                 []string                   `json:"issueTypes"`
	MaxBlockingTokenExpiration string                     `json:"maxBlockingTokenExpiration,omitempty"`
	PodCount                   int                        `json:"podCount"`
	SamplePods                 []string                   `json:"samplePods"`
	LegacyTokenSecrets         []legacyTokenSecretFinding `json:"legacyTokenSecrets,omitempty"`
	Pods                       []podFindingDetails        `json:"pods,omitempty"`
}

type podFindingDetails struct {
	Name               string                     `json:"name"`
	ServiceAccount     string                     `json:"serviceAccount"`
	Issues             []podIssueFinding          `json:"issues"`
	LegacyTokenSecrets []legacyTokenSecretFinding `json:"legacyTokenSecrets,omitempty"`
}

type podIssueFinding struct {
	Type                string   `json:"type"`
	Message             string   `json:"message"`
	Container           string   `json:"container,omitempty"`
	Path                string   `json:"path,omitempty"`
	Source              string   `json:"source,omitempty"`
	Secret              string   `json:"secret,omitempty"`
	TokenServiceAccount string   `json:"tokenServiceAccount,omitempty"`
	IssuedAt            string   `json:"issuedAt,omitempty"`
	ExpiresAt           string   `json:"expiresAt,omitempty"`
	WaitUntil           string   `json:"waitUntil,omitempty"`
	ReferenceSources    []string `json:"referenceSources,omitempty"`

	expiresAt *time.Time
}

type legacyTokenSecretFinding struct {
	Namespace         string   `json:"namespace"`
	Name              string   `json:"name"`
	ServiceAccount    string   `json:"serviceAccount,omitempty"`
	ServiceAccountUID string   `json:"serviceAccountUID,omitempty"`
	HasToken          bool     `json:"hasToken"`
	LastUsed          string   `json:"lastUsed,omitempty"`
	InvalidSince      string   `json:"invalidSince,omitempty"`
	ReferenceSources  []string `json:"referenceSources,omitempty"`
}

type ownerKey struct {
	Namespace string
	Kind      string
	Name      string
}

type legacyTokenSecret struct {
	Namespace         string
	Name              string
	ServiceAccount    string
	ServiceAccountUID string
	HasToken          bool
	LastUsed          string
	InvalidSince      string
}

type podSecretReference struct {
	key     types.NamespacedName
	sources map[string]struct{}
}

type podFileCandidate struct {
	container string
	path      string
	source    string
}

type appAccumulator struct {
	namespace                  string
	ownerKind                  string
	ownerName                  string
	serviceAccounts            map[string]struct{}
	issueTypes                 map[string]struct{}
	pods                       map[string]podFindingDetails
	legacyTokenSecrets         map[types.NamespacedName]*legacyTokenSecretAccumulator
	maxBlockingTokenExpiration *time.Time
}

type legacyTokenSecretAccumulator struct {
	secret           legacyTokenSecret
	referenceSources map[string]struct{}
}

type podFileReader interface {
	ReadFile(ctx context.Context, pod corev1.Pod, container, filePath string) ([]byte, error)
}

type execPodFileReader struct {
	config *rest.Config
	client kubernetes.Interface
}

type jwtTimestamps struct {
	IssuedAt  *time.Time
	ExpiresAt *time.Time
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	opts, err := parseOptions(args, stdout)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	config, err := buildConfig(opts.kubeconfig, opts.contextName)
	if err != nil {
		return err
	}
	config.UserAgent = appName

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create kubernetes client: %w", err)
	}

	var reader podFileReader
	if opts.needsPodExec() {
		reader = execPodFileReader{config: config, client: client}
	}

	result, err := scanCluster(ctx, client, reader, opts)
	if err != nil {
		return err
	}

	for _, warning := range result.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", warning)
	}

	return writeResult(stdout, result, opts)
}

func parseOptions(args []string, output io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet(appName, flag.ContinueOnError)
	fs.SetOutput(output)

	defaultKubeconfig := ""
	if home := homedir.HomeDir(); home != "" {
		defaultKubeconfig = filepath.Join(home, ".kube", "config")
	}

	fs.StringVar(&opts.kubeconfig, "kubeconfig", defaultKubeconfig, "path to kubeconfig; falls back to in-cluster config when unavailable")
	fs.StringVar(&opts.contextName, "context", "", "kubeconfig context to use")
	fs.StringVar(&opts.namespace, "namespace", metav1.NamespaceAll, "namespace to scan; empty means all namespaces")
	fs.StringVar(&opts.output, "output", "csv", "output format: csv, table, or json")
	fs.IntVar(&opts.maxSamples, "max-samples", 5, "maximum pod names to show per application in table output")
	fs.BoolVar(&opts.includePodBreakdown, "include-pods", false, "include per-Pod details in JSON output")
	fs.BoolVar(&opts.skipPodExec, "skip-pod-exec", false, "skip reading files inside Pods")
	fs.BoolVar(&opts.skipLegacySecretInspection, "skip-legacy-secret-inspection", false, "skip listing Secrets for legacy service-account-token checks")
	fs.StringVar(&opts.saKeyCutoverText, "sa-key-cutover", "", "RFC3339 time when the new ServiceAccount signing key started issuing tokens")
	fs.StringVar(&opts.newCAFile, "new-ca-file", "", "PEM file containing the required new CA certificate material")
	fs.StringVar(&opts.caCertPath, "ca-cert-path", defaultCACertPath, "path to ca.crt inside containers")
	fs.StringVar(&opts.tokenPath, "token-path", defaultTokenPath, "path to the default projected ServiceAccount token inside containers")

	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() > 0 {
		return opts, fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}
	if opts.output != "csv" && opts.output != "table" && opts.output != "json" {
		return opts, fmt.Errorf("unsupported output %q; use csv, table, or json", opts.output)
	}
	if opts.maxSamples < 1 {
		return opts, errors.New("--max-samples must be greater than zero")
	}
	if !strings.HasPrefix(opts.caCertPath, "/") {
		return opts, errors.New("--ca-cert-path must be an absolute container path")
	}
	if !strings.HasPrefix(opts.tokenPath, "/") {
		return opts, errors.New("--token-path must be an absolute container path")
	}
	if opts.saKeyCutoverText != "" {
		cutover, err := time.Parse(time.RFC3339, opts.saKeyCutoverText)
		if err != nil {
			return opts, fmt.Errorf("parse --sa-key-cutover: %w", err)
		}
		cutover = cutover.UTC()
		opts.saKeyCutover = &cutover
	}
	if opts.newCAFile != "" {
		data, err := os.ReadFile(opts.newCAFile)
		if err != nil {
			return opts, fmt.Errorf("read --new-ca-file: %w", err)
		}
		fingerprints, err := certificateFingerprints(data)
		if err != nil {
			return opts, fmt.Errorf("parse --new-ca-file: %w", err)
		}
		opts.requiredCAFingerprints = fingerprints
	}
	return opts, nil
}

func (opts options) needsPodExec() bool {
	if opts.skipPodExec {
		return false
	}
	return opts.saKeyCutover != nil || len(opts.requiredCAFingerprints) > 0
}

func buildConfig(kubeconfigPath, contextName string) (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		loadingRules.ExplicitPath = kubeconfigPath
	}

	overrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		overrides.CurrentContext = contextName
	}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err == nil {
		return config, nil
	}

	if kubeconfigPath == "" || !fileExists(kubeconfigPath) {
		inClusterConfig, inClusterErr := rest.InClusterConfig()
		if inClusterErr == nil {
			return inClusterConfig, nil
		}
		return nil, fmt.Errorf("load kubeconfig or in-cluster config: kubeconfig: %v; in-cluster: %v", err, inClusterErr)
	}

	return nil, fmt.Errorf("load kubeconfig: %w", err)
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func scanCluster(ctx context.Context, client kubernetes.Interface, reader podFileReader, opts options) (scanResult, error) {
	var result scanResult

	if opts.now.IsZero() {
		opts.now = time.Now().UTC()
	}

	serviceAccountAutomounts, err := loadServiceAccountAutomounts(ctx, client, opts.namespace)
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("could not list ServiceAccounts; assuming automount defaults for pods without pod-level automountServiceAccountToken: %v", err))
	}

	legacyTokenSecrets := map[types.NamespacedName]legacyTokenSecret{}
	if !opts.skipLegacySecretInspection {
		legacyTokenSecrets, err = loadLegacyServiceAccountTokenSecrets(ctx, client, opts.namespace)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("could not inspect Secrets; legacy service-account-token checks may be incomplete: %v", err))
		}
	}

	pods, err := client.CoreV1().Pods(opts.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return result, fmt.Errorf("list pods: %w", err)
	}

	if (opts.saKeyCutover != nil || len(opts.requiredCAFingerprints) > 0) && reader == nil {
		result.Warnings = append(result.Warnings, "pod exec inspection is disabled; ca.crt and projected token checks were skipped")
	}

	apps := map[ownerKey]*appAccumulator{}
	referencedLegacyTokenSecrets := map[types.NamespacedName]struct{}{}

	for _, pod := range pods.Items {
		podFinding := inspectPod(ctx, reader, pod, serviceAccountAutomounts, legacyTokenSecrets, opts)
		if len(podFinding.Issues) == 0 {
			continue
		}

		owner := resolvePodOwner(pod)
		app := apps[owner]
		if app == nil {
			app = &appAccumulator{
				namespace:          owner.Namespace,
				ownerKind:          owner.Kind,
				ownerName:          owner.Name,
				serviceAccounts:    map[string]struct{}{},
				issueTypes:         map[string]struct{}{},
				pods:               map[string]podFindingDetails{},
				legacyTokenSecrets: map[types.NamespacedName]*legacyTokenSecretAccumulator{},
			}
			apps[owner] = app
		}

		app.serviceAccounts[podFinding.ServiceAccount] = struct{}{}
		for _, issue := range podFinding.Issues {
			app.issueTypes[issue.Type] = struct{}{}
			if issue.expiresAt != nil && (app.maxBlockingTokenExpiration == nil || issue.expiresAt.After(*app.maxBlockingTokenExpiration)) {
				expiration := *issue.expiresAt
				app.maxBlockingTokenExpiration = &expiration
			}
		}
		for _, tokenSecret := range podFinding.LegacyTokenSecrets {
			key := namespacedName(tokenSecret.Namespace, tokenSecret.Name)
			referencedLegacyTokenSecrets[key] = struct{}{}
			secretAcc := app.legacyTokenSecrets[key]
			if secretAcc == nil {
				secretAcc = &legacyTokenSecretAccumulator{
					secret: legacyTokenSecret{
						Namespace:         tokenSecret.Namespace,
						Name:              tokenSecret.Name,
						ServiceAccount:    tokenSecret.ServiceAccount,
						ServiceAccountUID: tokenSecret.ServiceAccountUID,
						HasToken:          tokenSecret.HasToken,
						LastUsed:          tokenSecret.LastUsed,
						InvalidSince:      tokenSecret.InvalidSince,
					},
					referenceSources: map[string]struct{}{},
				}
				app.legacyTokenSecrets[key] = secretAcc
			}
			for _, source := range tokenSecret.ReferenceSources {
				secretAcc.referenceSources[source] = struct{}{}
			}
		}
		app.pods[podFinding.Name] = podFinding
	}

	result.Items = flattenApplications(apps, opts)
	result.UnreferencedLegacyTokenSecrets = unreferencedLegacyTokenSecrets(legacyTokenSecrets, referencedLegacyTokenSecrets)
	return result, nil
}

func inspectPod(ctx context.Context, reader podFileReader, pod corev1.Pod, serviceAccountAutomounts map[types.NamespacedName]*bool, legacyTokenSecrets map[types.NamespacedName]legacyTokenSecret, opts options) podFindingDetails {
	finding := podFindingDetails{
		Name:           pod.Name,
		ServiceAccount: podServiceAccount(pod),
	}

	for _, issue := range inspectLegacyTokenSecretUse(pod, legacyTokenSecrets) {
		finding.Issues = append(finding.Issues, issue)
	}
	finding.LegacyTokenSecrets = collectReferencedLegacyTokenSecrets(pod, legacyTokenSecrets)

	if reader == nil {
		sortPodFinding(&finding)
		return finding
	}

	containers := inspectableContainers(pod)
	if len(containers) == 0 && (opts.saKeyCutover != nil || len(opts.requiredCAFingerprints) > 0) {
		finding.Issues = append(finding.Issues, podIssueFinding{
			Type:    "pod_not_inspectable",
			Message: "Pod has no running regular container to exec into; current ca.crt and projected token could not be confirmed",
		})
		sortPodFinding(&finding)
		return finding
	}

	for _, container := range containers {
		if len(opts.requiredCAFingerprints) > 0 && shouldInspectContainerPath(pod, container, opts.caCertPath, serviceAccountAutomounts) {
			finding.Issues = append(finding.Issues, inspectContainerCACert(ctx, reader, pod, container.Name, opts.caCertPath, opts.requiredCAFingerprints)...)
		}

		if opts.saKeyCutover != nil {
			for _, candidate := range collectServiceAccountTokenCandidates(pod, container, opts.tokenPath, serviceAccountAutomounts) {
				finding.Issues = append(finding.Issues, inspectProjectedToken(ctx, reader, pod, candidate, *opts.saKeyCutover, opts.now)...)
			}
		}
	}

	sortPodFinding(&finding)
	return finding
}

func inspectLegacyTokenSecretUse(pod corev1.Pod, legacyTokenSecrets map[types.NamespacedName]legacyTokenSecret) []podIssueFinding {
	refs := collectPodSecretReferences(pod)
	issues := []podIssueFinding{}
	for _, key := range sortedNamespacedReferenceKeys(refs) {
		tokenSecret, ok := legacyTokenSecrets[key]
		if !ok {
			continue
		}
		issues = append(issues, podIssueFinding{
			Type:                "legacy_secret_token_in_use",
			Message:             "legacy kubernetes.io/service-account-token Secret is referenced by this Pod and usually does not expire naturally",
			Secret:              tokenSecret.Name,
			TokenServiceAccount: tokenSecret.ServiceAccount,
			ReferenceSources:    sortedSet(refs[key].sources),
		})
	}
	return issues
}

func collectReferencedLegacyTokenSecrets(pod corev1.Pod, legacyTokenSecrets map[types.NamespacedName]legacyTokenSecret) []legacyTokenSecretFinding {
	refs := collectPodSecretReferences(pod)
	items := []legacyTokenSecretFinding{}
	for _, key := range sortedNamespacedReferenceKeys(refs) {
		tokenSecret, ok := legacyTokenSecrets[key]
		if !ok {
			continue
		}
		items = append(items, tokenSecret.toFinding(refs[key].sources))
	}
	return items
}

func inspectContainerCACert(ctx context.Context, reader podFileReader, pod corev1.Pod, containerName, filePath string, required map[string]struct{}) []podIssueFinding {
	data, err := reader.ReadFile(ctx, pod, containerName, filePath)
	if err != nil {
		return []podIssueFinding{{
			Type:      "ca_crt_read_failed",
			Message:   fmt.Sprintf("could not read ca.crt to confirm the new CA is trusted: %v", err),
			Container: containerName,
			Path:      filePath,
		}}
	}

	actual, err := certificateFingerprints(data)
	if err != nil {
		return []podIssueFinding{{
			Type:      "ca_crt_parse_failed",
			Message:   fmt.Sprintf("could not parse ca.crt as certificate material: %v", err),
			Container: containerName,
			Path:      filePath,
		}}
	}

	if containsAllFingerprints(actual, required) {
		return nil
	}
	return []podIssueFinding{{
		Type:      "ca_crt_missing_required_ca",
		Message:   "ca.crt does not contain every certificate from --new-ca-file",
		Container: containerName,
		Path:      filePath,
	}}
}

func inspectProjectedToken(ctx context.Context, reader podFileReader, pod corev1.Pod, candidate podFileCandidate, cutover, now time.Time) []podIssueFinding {
	data, err := reader.ReadFile(ctx, pod, candidate.container, candidate.path)
	if err != nil {
		return []podIssueFinding{{
			Type:      "projected_token_read_failed",
			Message:   fmt.Sprintf("could not read projected ServiceAccount token to confirm it was issued after cutover: %v", err),
			Container: candidate.container,
			Path:      candidate.path,
			Source:    candidate.source,
		}}
	}

	timestamps, err := decodeJWTTimestamps(strings.TrimSpace(string(data)))
	if err != nil {
		return []podIssueFinding{{
			Type:      "projected_token_decode_failed",
			Message:   fmt.Sprintf("could not decode projected ServiceAccount token timestamps: %v", err),
			Container: candidate.container,
			Path:      candidate.path,
			Source:    candidate.source,
		}}
	}
	if timestamps.IssuedAt == nil {
		return []podIssueFinding{{
			Type:      "projected_token_missing_iat",
			Message:   "projected ServiceAccount token has no iat claim; cannot confirm it was issued after cutover",
			Container: candidate.container,
			Path:      candidate.path,
			Source:    candidate.source,
		}}
	}
	if timestamps.IssuedAt.After(cutover) {
		return nil
	}
	if timestamps.ExpiresAt != nil && !timestamps.ExpiresAt.After(now) {
		return nil
	}

	issue := podIssueFinding{
		Type:      "projected_token_issued_before_cutover",
		Message:   "projected ServiceAccount token was issued before --sa-key-cutover and may still require old sa.pub verification",
		Container: candidate.container,
		Path:      candidate.path,
		Source:    candidate.source,
		IssuedAt:  formatTime(*timestamps.IssuedAt),
	}
	if timestamps.ExpiresAt != nil {
		expiration := *timestamps.ExpiresAt
		issue.ExpiresAt = formatTime(expiration)
		issue.WaitUntil = issue.ExpiresAt
		issue.expiresAt = &expiration
	}
	return []podIssueFinding{issue}
}

func loadServiceAccountAutomounts(ctx context.Context, client kubernetes.Interface, namespace string) (map[types.NamespacedName]*bool, error) {
	serviceAccounts, err := client.CoreV1().ServiceAccounts(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	automounts := map[types.NamespacedName]*bool{}
	for _, serviceAccount := range serviceAccounts.Items {
		automounts[namespacedName(serviceAccount.Namespace, serviceAccount.Name)] = serviceAccount.AutomountServiceAccountToken
	}
	return automounts, nil
}

func loadLegacyServiceAccountTokenSecrets(ctx context.Context, client kubernetes.Interface, namespace string) (map[types.NamespacedName]legacyTokenSecret, error) {
	secrets, err := client.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	tokenSecrets := map[types.NamespacedName]legacyTokenSecret{}
	for _, secret := range secrets.Items {
		tokenSecret, ok := detectLegacyServiceAccountTokenSecret(secret)
		if !ok {
			continue
		}
		tokenSecrets[namespacedName(tokenSecret.Namespace, tokenSecret.Name)] = tokenSecret
	}
	return tokenSecrets, nil
}

func detectLegacyServiceAccountTokenSecret(secret corev1.Secret) (legacyTokenSecret, bool) {
	if secret.Type != corev1.SecretTypeServiceAccountToken {
		return legacyTokenSecret{}, false
	}

	annotations := secret.Annotations
	tokenSecret := legacyTokenSecret{
		Namespace:         secret.Namespace,
		Name:              secret.Name,
		ServiceAccount:    annotations[legacyTokenServiceAccountKey],
		ServiceAccountUID: annotations[legacyTokenServiceAccountUIDKey],
		HasToken:          len(secret.Data[corev1.ServiceAccountTokenKey]) > 0,
		LastUsed:          annotations[legacyTokenLastUsedAnnotation],
		InvalidSince:      annotations[legacyTokenInvalidAnnotation],
	}
	return tokenSecret, true
}

func collectPodSecretReferences(pod corev1.Pod) map[types.NamespacedName]podSecretReference {
	refs := map[types.NamespacedName]podSecretReference{}

	add := func(name, source string) {
		if name == "" {
			return
		}
		key := namespacedName(pod.Namespace, name)
		ref := refs[key]
		if ref.sources == nil {
			ref = podSecretReference{
				key:     key,
				sources: map[string]struct{}{},
			}
		}
		ref.sources[source] = struct{}{}
		refs[key] = ref
	}

	for _, volume := range pod.Spec.Volumes {
		if volume.Secret != nil {
			add(volume.Secret.SecretName, fmt.Sprintf("volume %q", volume.Name))
		}
		if volume.Projected != nil {
			for _, source := range volume.Projected.Sources {
				if source.Secret != nil {
					add(source.Secret.Name, fmt.Sprintf("projected volume %q", volume.Name))
				}
			}
		}
	}

	for _, container := range pod.Spec.InitContainers {
		collectContainerSecretReferences(add, "initContainer", container.Name, container.Env, container.EnvFrom)
	}
	for _, container := range pod.Spec.Containers {
		collectContainerSecretReferences(add, "container", container.Name, container.Env, container.EnvFrom)
	}
	for _, container := range pod.Spec.EphemeralContainers {
		collectContainerSecretReferences(add, "ephemeralContainer", container.Name, container.Env, container.EnvFrom)
	}

	return refs
}

func collectContainerSecretReferences(add func(string, string), containerType, containerName string, env []corev1.EnvVar, envFrom []corev1.EnvFromSource) {
	containerLabel := fmt.Sprintf("%s %q", containerType, containerName)

	for _, source := range envFrom {
		if source.SecretRef != nil {
			add(source.SecretRef.Name, containerLabel+" envFrom")
		}
	}

	for _, variable := range env {
		if variable.ValueFrom == nil || variable.ValueFrom.SecretKeyRef == nil {
			continue
		}
		add(variable.ValueFrom.SecretKeyRef.Name, fmt.Sprintf("%s env %q", containerLabel, variable.Name))
	}
}

func inspectableContainers(pod corev1.Pod) []corev1.Container {
	if len(pod.Status.ContainerStatuses) > 0 {
		running := map[string]struct{}{}
		for _, status := range pod.Status.ContainerStatuses {
			if status.State.Running != nil {
				running[status.Name] = struct{}{}
			}
		}

		containers := []corev1.Container{}
		for _, container := range pod.Spec.Containers {
			if _, ok := running[container.Name]; ok {
				containers = append(containers, container)
			}
		}
		return containers
	}

	if pod.Status.Phase != "" && pod.Status.Phase != corev1.PodRunning {
		return nil
	}
	return append([]corev1.Container(nil), pod.Spec.Containers...)
}

func shouldInspectContainerPath(pod corev1.Pod, container corev1.Container, filePath string, serviceAccountAutomounts map[types.NamespacedName]*bool) bool {
	if containerMountsPath(container, filePath) {
		return true
	}
	return effectiveAutomount(pod, serviceAccountAutomounts)
}

func collectServiceAccountTokenCandidates(pod corev1.Pod, container corev1.Container, defaultTokenPath string, serviceAccountAutomounts map[types.NamespacedName]*bool) []podFileCandidate {
	candidates := map[string]podFileCandidate{}

	if shouldInspectContainerPath(pod, container, defaultTokenPath, serviceAccountAutomounts) {
		candidates[defaultTokenPath] = podFileCandidate{
			container: container.Name,
			path:      defaultTokenPath,
			source:    "effective automountServiceAccountToken",
		}
	}

	for _, volume := range pod.Spec.Volumes {
		if volume.Projected == nil {
			continue
		}
		mount, ok := volumeMountByName(container, volume.Name)
		if !ok {
			continue
		}
		for _, source := range volume.Projected.Sources {
			if source.ServiceAccountToken == nil {
				continue
			}
			tokenPath := cleanProjectedPath(mount.MountPath, source.ServiceAccountToken.Path)
			candidates[tokenPath] = podFileCandidate{
				container: container.Name,
				path:      tokenPath,
				source:    fmt.Sprintf("projected volume %q", volume.Name),
			}
		}
	}

	paths := sortedKeys(candidates)
	items := make([]podFileCandidate, 0, len(paths))
	for _, candidatePath := range paths {
		items = append(items, candidates[candidatePath])
	}
	return items
}

func volumeMountByName(container corev1.Container, name string) (corev1.VolumeMount, bool) {
	for _, mount := range container.VolumeMounts {
		if mount.Name == name {
			return mount, true
		}
	}
	return corev1.VolumeMount{}, false
}

func containerMountsPath(container corev1.Container, filePath string) bool {
	filePath = path.Clean(filePath)
	for _, mount := range container.VolumeMounts {
		mountPath := path.Clean(mount.MountPath)
		if filePath == mountPath || strings.HasPrefix(filePath, mountPath+"/") {
			return true
		}
	}
	return false
}

func cleanProjectedPath(mountPath, projectedPath string) string {
	projectedPath = strings.TrimPrefix(projectedPath, "/")
	if projectedPath == "" {
		projectedPath = "token"
	}
	return path.Clean(path.Join(mountPath, projectedPath))
}

func effectiveAutomount(pod corev1.Pod, serviceAccountAutomounts map[types.NamespacedName]*bool) bool {
	if pod.Spec.AutomountServiceAccountToken != nil {
		return *pod.Spec.AutomountServiceAccountToken
	}

	serviceAccount := podServiceAccount(pod)
	if serviceAccountAutomounts != nil {
		if automount, ok := serviceAccountAutomounts[namespacedName(pod.Namespace, serviceAccount)]; ok && automount != nil {
			return *automount
		}
	}

	return true
}

func resolvePodOwner(pod corev1.Pod) ownerKey {
	owner := podOwner(&pod.ObjectMeta)
	if owner == nil {
		return ownerKey{Namespace: pod.Namespace, Kind: "Pod", Name: pod.Name}
	}
	return ownerKey{Namespace: pod.Namespace, Kind: owner.Kind, Name: owner.Name}
}

func podOwner(meta *metav1.ObjectMeta) *metav1.OwnerReference {
	ref := metav1.GetControllerOf(meta)
	if ref != nil {
		copied := *ref
		return &copied
	}
	if len(meta.OwnerReferences) == 0 {
		return nil
	}
	copied := meta.OwnerReferences[0]
	return &copied
}

func podServiceAccount(pod corev1.Pod) string {
	if pod.Spec.ServiceAccountName == "" {
		return defaultServiceAccount
	}
	return pod.Spec.ServiceAccountName
}

func decodeJWTTimestamps(token string) (jwtTimestamps, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return jwtTimestamps{}, errors.New("token is not a JWT")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payloadBytes, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return jwtTimestamps{}, fmt.Errorf("decode JWT payload: %w", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(payloadBytes))
	decoder.UseNumber()
	payload := map[string]interface{}{}
	if err := decoder.Decode(&payload); err != nil {
		return jwtTimestamps{}, fmt.Errorf("parse JWT payload: %w", err)
	}

	issuedAt, _, err := readUnixTimeClaim(payload, "iat")
	if err != nil {
		return jwtTimestamps{}, err
	}
	expiresAt, _, err := readUnixTimeClaim(payload, "exp")
	if err != nil {
		return jwtTimestamps{}, err
	}
	return jwtTimestamps{IssuedAt: issuedAt, ExpiresAt: expiresAt}, nil
}

func readUnixTimeClaim(payload map[string]interface{}, key string) (*time.Time, bool, error) {
	raw, ok := payload[key]
	if !ok {
		return nil, false, nil
	}

	var value float64
	switch typed := raw.(type) {
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return nil, true, fmt.Errorf("parse %s claim: %w", key, err)
		}
		value = parsed
	case float64:
		value = typed
	case string:
		parsed, err := strconv.ParseFloat(typed, 64)
		if err != nil {
			return nil, true, fmt.Errorf("parse %s claim: %w", key, err)
		}
		value = parsed
	default:
		return nil, true, fmt.Errorf("claim %s is %T, expected number", key, raw)
	}

	claimTime := time.Unix(int64(value), 0).UTC()
	return &claimTime, true, nil
}

func certificateFingerprints(data []byte) (map[string]struct{}, error) {
	fingerprints := map[string]struct{}{}
	remaining := bytes.TrimSpace(data)

	for len(remaining) > 0 {
		block, rest := pem.Decode(remaining)
		if block == nil {
			break
		}
		remaining = bytes.TrimSpace(rest)
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		fingerprints[certificateFingerprint(cert.Raw)] = struct{}{}
	}
	if len(fingerprints) > 0 {
		return fingerprints, nil
	}

	certs, err := x509.ParseCertificates(data)
	if err != nil {
		return nil, err
	}
	for _, cert := range certs {
		fingerprints[certificateFingerprint(cert.Raw)] = struct{}{}
	}
	if len(fingerprints) == 0 {
		return nil, errors.New("no certificates found")
	}
	return fingerprints, nil
}

func certificateFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func containsAllFingerprints(actual, required map[string]struct{}) bool {
	for fingerprint := range required {
		if _, ok := actual[fingerprint]; !ok {
			return false
		}
	}
	return true
}

func flattenApplications(apps map[ownerKey]*appAccumulator, opts options) []applicationFinding {
	items := make([]applicationFinding, 0, len(apps))
	for _, app := range apps {
		podNames := sortedKeys(app.pods)
		item := applicationFinding{
			Namespace:          app.namespace,
			OwnerKind:          app.ownerKind,
			OwnerName:          app.ownerName,
			ServiceAccounts:    sortedSet(app.serviceAccounts),
			IssueTypes:         sortedSet(app.issueTypes),
			PodCount:           len(app.pods),
			SamplePods:         trimStrings(podNames, opts.maxSamples),
			LegacyTokenSecrets: flattenLegacyTokenSecretAccumulators(app.legacyTokenSecrets),
		}
		if app.maxBlockingTokenExpiration != nil {
			item.MaxBlockingTokenExpiration = formatTime(*app.maxBlockingTokenExpiration)
		}
		if opts.includePodBreakdown {
			item.Pods = make([]podFindingDetails, 0, len(podNames))
			for _, podName := range podNames {
				item.Pods = append(item.Pods, app.pods[podName])
			}
		}
		items = append(items, item)
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Namespace != items[j].Namespace {
			return items[i].Namespace < items[j].Namespace
		}
		if items[i].OwnerKind != items[j].OwnerKind {
			return items[i].OwnerKind < items[j].OwnerKind
		}
		return items[i].OwnerName < items[j].OwnerName
	})
	return items
}

func flattenLegacyTokenSecretAccumulators(tokenSecrets map[types.NamespacedName]*legacyTokenSecretAccumulator) []legacyTokenSecretFinding {
	keys := sortedLegacyTokenSecretAccumulatorKeys(tokenSecrets)
	items := make([]legacyTokenSecretFinding, 0, len(keys))
	for _, key := range keys {
		items = append(items, tokenSecrets[key].toFinding())
	}
	return items
}

func unreferencedLegacyTokenSecrets(tokenSecrets map[types.NamespacedName]legacyTokenSecret, referenced map[types.NamespacedName]struct{}) []legacyTokenSecretFinding {
	keys := sortedLegacyTokenSecretKeys(tokenSecrets)
	items := []legacyTokenSecretFinding{}
	for _, key := range keys {
		if _, ok := referenced[key]; ok {
			continue
		}
		items = append(items, tokenSecrets[key].toFinding(nil))
	}
	return items
}

func (secret legacyTokenSecret) toFinding(referenceSources map[string]struct{}) legacyTokenSecretFinding {
	return legacyTokenSecretFinding{
		Namespace:         secret.Namespace,
		Name:              secret.Name,
		ServiceAccount:    secret.ServiceAccount,
		ServiceAccountUID: secret.ServiceAccountUID,
		HasToken:          secret.HasToken,
		LastUsed:          secret.LastUsed,
		InvalidSince:      secret.InvalidSince,
		ReferenceSources:  sortedSet(referenceSources),
	}
}

func (secret *legacyTokenSecretAccumulator) toFinding() legacyTokenSecretFinding {
	return secret.secret.toFinding(secret.referenceSources)
}

func writeResult(w io.Writer, result scanResult, opts options) error {
	switch opts.output {
	case "csv":
		return writeCSV(w, result)
	case "json":
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	case "table":
		return writeTable(w, result)
	default:
		return fmt.Errorf("unsupported output %q", opts.output)
	}
}

func writeCSV(w io.Writer, result scanResult) error {
	writer := csv.NewWriter(w)
	if err := writer.Write([]string{"namespace", "ownerKind", "ownerName", "serviceAccounts", "issueTypes", "maxBlockingTokenExpiration", "legacyTokenSecrets", "pods"}); err != nil {
		return err
	}
	for _, item := range result.Items {
		if err := writer.Write([]string{
			item.Namespace,
			item.OwnerKind,
			item.OwnerName,
			strings.Join(item.ServiceAccounts, ","),
			strings.Join(item.IssueTypes, ","),
			item.MaxBlockingTokenExpiration,
			formatLegacyTokenSecretNames(item.LegacyTokenSecrets),
			strings.Join(item.SamplePods, ","),
		}); err != nil {
			return err
		}
	}
	writer.Flush()
	return writer.Error()
}

func writeTable(w io.Writer, result scanResult) error {
	if len(result.Items) == 0 {
		if _, err := fmt.Fprintln(w, "No CA or ServiceAccount token retirement blockers were found."); err != nil {
			return err
		}
		if len(result.UnreferencedLegacyTokenSecrets) > 0 {
			_, err := fmt.Fprintf(w, "Unreferenced legacy ServiceAccount token Secrets: %d. Use --output json to inspect them.\n", len(result.UnreferencedLegacyTokenSecrets))
			return err
		}
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAMESPACE\tOWNER\tSERVICE_ACCOUNTS\tISSUES\tMAX_TOKEN_EXPIRATION\tLEGACY_TOKEN_SECRETS\tPODS\tSAMPLE_PODS")
	for _, item := range result.Items {
		fmt.Fprintf(
			tw,
			"%s\t%s/%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			item.Namespace,
			item.OwnerKind,
			item.OwnerName,
			strings.Join(item.ServiceAccounts, ","),
			strings.Join(item.IssueTypes, ","),
			item.MaxBlockingTokenExpiration,
			formatLegacyTokenSecrets(item.LegacyTokenSecrets),
			item.PodCount,
			strings.Join(item.SamplePods, ","),
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(result.UnreferencedLegacyTokenSecrets) > 0 {
		_, err := fmt.Fprintf(w, "\nUnreferenced legacy ServiceAccount token Secrets: %d. Use --output json to inspect them.\n", len(result.UnreferencedLegacyTokenSecrets))
		return err
	}
	return nil
}

func (reader execPodFileReader) ReadFile(ctx context.Context, pod corev1.Pod, container, filePath string) ([]byte, error) {
	request := reader.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(pod.Namespace).
		Name(pod.Name).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   []string{"cat", filePath},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(reader.config, http.MethodPost, request.URL())
	if err != nil {
		return nil, err
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return nil, fmt.Errorf("%w: %s", err, message)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

func formatLegacyTokenSecretNames(tokenSecrets []legacyTokenSecretFinding) string {
	parts := make([]string, 0, len(tokenSecrets))
	for _, tokenSecret := range tokenSecrets {
		parts = append(parts, tokenSecret.Name)
	}
	return strings.Join(parts, ",")
}

func formatLegacyTokenSecrets(tokenSecrets []legacyTokenSecretFinding) string {
	parts := make([]string, 0, len(tokenSecrets))
	for _, tokenSecret := range tokenSecrets {
		if tokenSecret.ServiceAccount == "" {
			parts = append(parts, "Secret/"+tokenSecret.Name)
			continue
		}
		parts = append(parts, fmt.Sprintf("Secret/%s(sa:%s)", tokenSecret.Name, tokenSecret.ServiceAccount))
	}
	return strings.Join(parts, "; ")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedSet(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedLegacyTokenSecretKeys(tokenSecrets map[types.NamespacedName]legacyTokenSecret) []types.NamespacedName {
	keys := make([]types.NamespacedName, 0, len(tokenSecrets))
	for key := range tokenSecrets {
		keys = append(keys, key)
	}
	sortNamespacedNames(keys)
	return keys
}

func sortedLegacyTokenSecretAccumulatorKeys(tokenSecrets map[types.NamespacedName]*legacyTokenSecretAccumulator) []types.NamespacedName {
	keys := make([]types.NamespacedName, 0, len(tokenSecrets))
	for key := range tokenSecrets {
		keys = append(keys, key)
	}
	sortNamespacedNames(keys)
	return keys
}

func sortedNamespacedReferenceKeys(refs map[types.NamespacedName]podSecretReference) []types.NamespacedName {
	keys := make([]types.NamespacedName, 0, len(refs))
	for key := range refs {
		keys = append(keys, key)
	}
	sortNamespacedNames(keys)
	return keys
}

func sortNamespacedNames(keys []types.NamespacedName) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Namespace != keys[j].Namespace {
			return keys[i].Namespace < keys[j].Namespace
		}
		return keys[i].Name < keys[j].Name
	})
}

func sortPodFinding(finding *podFindingDetails) {
	sort.Slice(finding.Issues, func(i, j int) bool {
		if finding.Issues[i].Type != finding.Issues[j].Type {
			return finding.Issues[i].Type < finding.Issues[j].Type
		}
		if finding.Issues[i].Container != finding.Issues[j].Container {
			return finding.Issues[i].Container < finding.Issues[j].Container
		}
		if finding.Issues[i].Path != finding.Issues[j].Path {
			return finding.Issues[i].Path < finding.Issues[j].Path
		}
		return finding.Issues[i].Secret < finding.Issues[j].Secret
	})
	sort.Slice(finding.LegacyTokenSecrets, func(i, j int) bool {
		if finding.LegacyTokenSecrets[i].Namespace != finding.LegacyTokenSecrets[j].Namespace {
			return finding.LegacyTokenSecrets[i].Namespace < finding.LegacyTokenSecrets[j].Namespace
		}
		return finding.LegacyTokenSecrets[i].Name < finding.LegacyTokenSecrets[j].Name
	})
}

func trimStrings(values []string, max int) []string {
	if len(values) <= max {
		return values
	}
	return append([]string(nil), values[:max]...)
}

func namespacedName(namespace, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: name}
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339)
}
