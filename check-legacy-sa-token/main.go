package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

const (
	appName                         = "check-legacy-sa-token"
	defaultServiceAccount           = "default"
	legacyTokenLastUsedAnnotation   = "kubernetes.io/legacy-token-last-used"
	legacyTokenInvalidAnnotation    = "kubernetes.io/legacy-token-invalid-since"
	legacyTokenServiceAccountKey    = corev1.ServiceAccountNameKey
	legacyTokenServiceAccountUIDKey = corev1.ServiceAccountUIDKey
)

type options struct {
	kubeconfig          string
	contextName         string
	namespace           string
	output              string
	maxSamples          int
	includePodBreakdown bool
}

type scanResult struct {
	Items                    []applicationFinding `json:"items"`
	UnreferencedTokenSecrets []tokenSecretFinding `json:"unreferencedTokenSecrets,omitempty"`
	Warnings                 []string             `json:"warnings,omitempty"`
}

type applicationFinding struct {
	Namespace            string               `json:"namespace"`
	OwnerKind            string               `json:"ownerKind"`
	OwnerName            string               `json:"ownerName"`
	ServiceAccounts      []string             `json:"serviceAccounts"`
	TokenServiceAccounts []string             `json:"tokenServiceAccounts"`
	PodCount             int                  `json:"podCount"`
	SamplePods           []string             `json:"samplePods"`
	TokenSecrets         []tokenSecretFinding `json:"tokenSecrets"`
	Pods                 []podFindingDetails  `json:"pods,omitempty"`
}

type tokenSecretFinding struct {
	Namespace         string   `json:"namespace"`
	Name              string   `json:"name"`
	ServiceAccount    string   `json:"serviceAccount,omitempty"`
	ServiceAccountUID string   `json:"serviceAccountUID,omitempty"`
	HasToken          bool     `json:"hasToken"`
	LastUsed          string   `json:"lastUsed,omitempty"`
	InvalidSince      string   `json:"invalidSince,omitempty"`
	ReferenceSources  []string `json:"referenceSources,omitempty"`
}

type podFindingDetails struct {
	Name                 string               `json:"name"`
	ServiceAccount       string               `json:"serviceAccount"`
	TokenServiceAccounts []string             `json:"tokenServiceAccounts"`
	TokenSecrets         []tokenSecretFinding `json:"tokenSecrets"`
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

type appAccumulator struct {
	namespace            string
	ownerKind            string
	ownerName            string
	serviceAccounts      map[string]struct{}
	tokenServiceAccounts map[string]struct{}
	pods                 map[string]podFindingDetails
	tokenSecrets         map[types.NamespacedName]*tokenSecretAccumulator
}

type tokenSecretAccumulator struct {
	secret           legacyTokenSecret
	referenceSources map[string]struct{}
}

type podSecretReference struct {
	key     types.NamespacedName
	sources map[string]struct{}
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

	result, err := scanCluster(ctx, client, opts)
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
	fs.BoolVar(&opts.includePodBreakdown, "include-pods", false, "include per-pod details in JSON output")

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
	return opts, nil
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

func scanCluster(ctx context.Context, client kubernetes.Interface, opts options) (scanResult, error) {
	var result scanResult

	tokenSecrets, err := loadLegacyServiceAccountTokenSecrets(ctx, client, opts.namespace)
	if err != nil {
		return result, fmt.Errorf("list legacy service-account-token Secrets: %w", err)
	}

	pods, err := client.CoreV1().Pods(opts.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return result, fmt.Errorf("list pods: %w", err)
	}

	apps := map[ownerKey]*appAccumulator{}
	referencedTokenSecrets := map[types.NamespacedName]struct{}{}

	for _, pod := range pods.Items {
		refs := collectPodSecretReferences(pod)
		if len(refs) == 0 {
			continue
		}

		podServiceAccountName := podServiceAccount(pod)
		podTokenServiceAccounts := map[string]struct{}{}
		podTokenSecrets := []tokenSecretFinding{}
		var app *appAccumulator

		for key, ref := range refs {
			tokenSecret, ok := tokenSecrets[key]
			if !ok {
				continue
			}

			if app == nil {
				owner := resolvePodOwner(pod)
				app = apps[owner]
				if app == nil {
					app = &appAccumulator{
						namespace:            owner.Namespace,
						ownerKind:            owner.Kind,
						ownerName:            owner.Name,
						serviceAccounts:      map[string]struct{}{},
						tokenServiceAccounts: map[string]struct{}{},
						pods:                 map[string]podFindingDetails{},
						tokenSecrets:         map[types.NamespacedName]*tokenSecretAccumulator{},
					}
					apps[owner] = app
				}
			}

			referencedTokenSecrets[key] = struct{}{}
			app.serviceAccounts[podServiceAccountName] = struct{}{}
			if tokenSecret.ServiceAccount != "" {
				app.tokenServiceAccounts[tokenSecret.ServiceAccount] = struct{}{}
				podTokenServiceAccounts[tokenSecret.ServiceAccount] = struct{}{}
			}

			secretAcc := app.tokenSecrets[key]
			if secretAcc == nil {
				secretAcc = &tokenSecretAccumulator{
					secret:           tokenSecret,
					referenceSources: map[string]struct{}{},
				}
				app.tokenSecrets[key] = secretAcc
			}
			for source := range ref.sources {
				secretAcc.referenceSources[source] = struct{}{}
			}

			podTokenSecrets = append(podTokenSecrets, tokenSecret.toFinding(ref.sources))
		}

		if app != nil && len(podTokenSecrets) > 0 {
			sortTokenSecretFindings(podTokenSecrets)
			app.pods[pod.Name] = podFindingDetails{
				Name:                 pod.Name,
				ServiceAccount:       podServiceAccountName,
				TokenServiceAccounts: sortedSet(podTokenServiceAccounts),
				TokenSecrets:         podTokenSecrets,
			}
		}
	}

	result.Items = flattenApplications(apps, opts)
	result.UnreferencedTokenSecrets = unreferencedTokenSecrets(tokenSecrets, referencedTokenSecrets)
	return result, nil
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

func flattenApplications(apps map[ownerKey]*appAccumulator, opts options) []applicationFinding {
	items := make([]applicationFinding, 0, len(apps))
	for _, app := range apps {
		podNames := sortedKeys(app.pods)
		item := applicationFinding{
			Namespace:            app.namespace,
			OwnerKind:            app.ownerKind,
			OwnerName:            app.ownerName,
			ServiceAccounts:      sortedSet(app.serviceAccounts),
			TokenServiceAccounts: sortedSet(app.tokenServiceAccounts),
			PodCount:             len(app.pods),
			SamplePods:           trimStrings(podNames, opts.maxSamples),
			TokenSecrets:         flattenTokenSecretAccumulators(app.tokenSecrets),
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

func flattenTokenSecretAccumulators(tokenSecrets map[types.NamespacedName]*tokenSecretAccumulator) []tokenSecretFinding {
	keys := sortedTokenSecretAccumulatorKeys(tokenSecrets)
	items := make([]tokenSecretFinding, 0, len(keys))
	for _, key := range keys {
		items = append(items, tokenSecrets[key].toFinding())
	}
	return items
}

func unreferencedTokenSecrets(tokenSecrets map[types.NamespacedName]legacyTokenSecret, referenced map[types.NamespacedName]struct{}) []tokenSecretFinding {
	keys := sortedLegacyTokenSecretKeys(tokenSecrets)
	items := []tokenSecretFinding{}
	for _, key := range keys {
		if _, ok := referenced[key]; ok {
			continue
		}
		items = append(items, tokenSecrets[key].toFinding(nil))
	}
	return items
}

func (secret legacyTokenSecret) toFinding(referenceSources map[string]struct{}) tokenSecretFinding {
	return tokenSecretFinding{
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

func (secret *tokenSecretAccumulator) toFinding() tokenSecretFinding {
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
	if err := writer.Write([]string{"namespace", "ownerKind", "ownerName", "serviceAccounts", "tokenServiceAccounts", "tokenSecrets"}); err != nil {
		return err
	}
	for _, item := range result.Items {
		if err := writer.Write([]string{
			item.Namespace,
			item.OwnerKind,
			item.OwnerName,
			strings.Join(item.ServiceAccounts, ","),
			strings.Join(item.TokenServiceAccounts, ","),
			formatTokenSecretNames(item.TokenSecrets),
		}); err != nil {
			return err
		}
	}
	writer.Flush()
	return writer.Error()
}

func writeTable(w io.Writer, result scanResult) error {
	if len(result.Items) == 0 {
		if _, err := fmt.Fprintln(w, "No applications using legacy Secret-backed ServiceAccount tokens were found."); err != nil {
			return err
		}
		if len(result.UnreferencedTokenSecrets) > 0 {
			_, err := fmt.Fprintf(w, "Unreferenced legacy ServiceAccount token Secrets: %d. Use --output json to inspect them.\n", len(result.UnreferencedTokenSecrets))
			return err
		}
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAMESPACE\tOWNER\tSERVICE_ACCOUNTS\tTOKEN_SERVICE_ACCOUNTS\tTOKEN_SECRETS\tPODS\tSAMPLE_PODS")
	for _, item := range result.Items {
		fmt.Fprintf(
			tw,
			"%s\t%s/%s\t%s\t%s\t%s\t%d\t%s\n",
			item.Namespace,
			item.OwnerKind,
			item.OwnerName,
			strings.Join(item.ServiceAccounts, ","),
			strings.Join(item.TokenServiceAccounts, ","),
			formatTokenSecrets(item.TokenSecrets),
			item.PodCount,
			strings.Join(item.SamplePods, ","),
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(result.UnreferencedTokenSecrets) > 0 {
		_, err := fmt.Fprintf(w, "\nUnreferenced legacy ServiceAccount token Secrets: %d. Use --output json to inspect them.\n", len(result.UnreferencedTokenSecrets))
		return err
	}
	return nil
}

func formatTokenSecretNames(tokenSecrets []tokenSecretFinding) string {
	parts := make([]string, 0, len(tokenSecrets))
	for _, tokenSecret := range tokenSecrets {
		parts = append(parts, tokenSecret.Name)
	}
	return strings.Join(parts, ",")
}

func formatTokenSecrets(tokenSecrets []tokenSecretFinding) string {
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

func namespacedName(namespace, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: name}
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

func sortedTokenSecretAccumulatorKeys(m map[types.NamespacedName]*tokenSecretAccumulator) []types.NamespacedName {
	keys := make([]types.NamespacedName, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sortNamespacedNames(keys)
	return keys
}

func sortedLegacyTokenSecretKeys(m map[types.NamespacedName]legacyTokenSecret) []types.NamespacedName {
	keys := make([]types.NamespacedName, 0, len(m))
	for key := range m {
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

func sortTokenSecretFindings(items []tokenSecretFinding) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Namespace != items[j].Namespace {
			return items[i].Namespace < items[j].Namespace
		}
		return items[i].Name < items[j].Name
	})
}

func trimStrings(values []string, max int) []string {
	if len(values) <= max {
		return values
	}
	return append([]string(nil), values[:max]...)
}
