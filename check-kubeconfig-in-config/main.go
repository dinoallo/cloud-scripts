package main

import (
	"bytes"
	"context"
	"encoding/base64"
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

const appName = "check-kubeconfig-in-config"

type options struct {
	kubeconfig          string
	contextName         string
	namespace           string
	output              string
	maxSamples          int
	skipSecretInspect   bool
	includePodBreakdown bool
}

type scanResult struct {
	Items                 []applicationFinding    `json:"items"`
	UnreferencedResources []configResourceFinding `json:"unreferencedResources,omitempty"`
	Warnings              []string                `json:"warnings,omitempty"`
}

type applicationFinding struct {
	Namespace       string                  `json:"namespace"`
	OwnerKind       string                  `json:"ownerKind"`
	OwnerName       string                  `json:"ownerName"`
	PodCount        int                     `json:"podCount"`
	SamplePods      []string                `json:"samplePods"`
	ConfigResources []configResourceFinding `json:"configResources"`
	Pods            []podFindingDetails     `json:"pods,omitempty"`
}

type configResourceFinding struct {
	Namespace        string                 `json:"namespace"`
	Kind             string                 `json:"kind"`
	Name             string                 `json:"name"`
	KubeconfigKeys   []kubeconfigKeyFinding `json:"kubeconfigKeys"`
	ReferenceSources []string               `json:"referenceSources,omitempty"`
}

type kubeconfigKeyFinding struct {
	Key      string `json:"key"`
	Encoding string `json:"encoding"`
}

type podFindingDetails struct {
	Name            string                  `json:"name"`
	ConfigResources []configResourceFinding `json:"configResources"`
}

type ownerKey struct {
	Namespace string
	Kind      string
	Name      string
}

type configResourceKey struct {
	Namespace string
	Kind      string
	Name      string
}

type detectedConfigResource struct {
	Namespace      string
	Kind           string
	Name           string
	KubeconfigKeys []kubeconfigKeyFinding
}

type appAccumulator struct {
	namespace string
	ownerKind string
	ownerName string
	pods      map[string]podFindingDetails
	resources map[configResourceKey]*configResourceAccumulator
}

type configResourceAccumulator struct {
	resource         detectedConfigResource
	referenceSources map[string]struct{}
}

type podConfigReference struct {
	key     configResourceKey
	sources map[string]struct{}
}

type ownerResolver struct {
	replicaSetOwners map[types.NamespacedName]*metav1.OwnerReference
	jobOwners        map[types.NamespacedName]*metav1.OwnerReference
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
	fs.StringVar(&opts.output, "output", "table", "output format: table or json")
	fs.IntVar(&opts.maxSamples, "max-samples", 5, "maximum pod names to show per application in table output")
	fs.BoolVar(&opts.skipSecretInspect, "skip-secret-inspection", false, "skip listing Secrets")
	fs.BoolVar(&opts.includePodBreakdown, "include-pods", false, "include per-pod details in JSON output")

	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() > 0 {
		return opts, fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}
	if opts.output != "table" && opts.output != "json" {
		return opts, fmt.Errorf("unsupported output %q; use table or json", opts.output)
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

	configResources := map[configResourceKey]detectedConfigResource{}

	configMaps, err := loadKubeconfigConfigMaps(ctx, client, opts.namespace)
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("could not inspect ConfigMaps; ConfigMap kubeconfigs may be missed: %v", err))
	} else {
		mergeConfigResources(configResources, configMaps)
	}

	if !opts.skipSecretInspect {
		secrets, err := loadKubeconfigSecrets(ctx, client, opts.namespace)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("could not inspect Secrets; Secret kubeconfigs may be missed: %v", err))
		} else {
			mergeConfigResources(configResources, secrets)
		}
	}

	pods, err := client.CoreV1().Pods(opts.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return result, fmt.Errorf("list pods: %w", err)
	}

	resolver, ownerWarnings := loadOwnerResolver(ctx, client, opts.namespace)
	result.Warnings = append(result.Warnings, ownerWarnings...)

	apps := map[ownerKey]*appAccumulator{}
	referencedResources := map[configResourceKey]struct{}{}

	for _, pod := range pods.Items {
		refs := collectPodConfigReferences(pod)
		if len(refs) == 0 {
			continue
		}

		var app *appAccumulator
		podResources := []configResourceFinding{}
		for key, ref := range refs {
			resource, ok := configResources[key]
			if !ok {
				continue
			}

			if app == nil {
				owner := resolver.resolvePodOwner(pod)
				app = apps[owner]
				if app == nil {
					app = &appAccumulator{
						namespace: owner.Namespace,
						ownerKind: owner.Kind,
						ownerName: owner.Name,
						pods:      map[string]podFindingDetails{},
						resources: map[configResourceKey]*configResourceAccumulator{},
					}
					apps[owner] = app
				}
			}

			referencedResources[key] = struct{}{}
			resourceAcc := app.resources[key]
			if resourceAcc == nil {
				resourceAcc = &configResourceAccumulator{
					resource:         resource,
					referenceSources: map[string]struct{}{},
				}
				app.resources[key] = resourceAcc
			}
			for source := range ref.sources {
				resourceAcc.referenceSources[source] = struct{}{}
			}

			podResources = append(podResources, resource.toFinding(ref.sources))
		}

		if app != nil && len(podResources) > 0 {
			sortConfigResourceFindings(podResources)
			app.pods[pod.Name] = podFindingDetails{
				Name:            pod.Name,
				ConfigResources: podResources,
			}
		}
	}

	result.Items = flattenApplications(apps, opts)
	result.UnreferencedResources = unreferencedConfigResources(configResources, referencedResources)
	return result, nil
}

func mergeConfigResources(dst, src map[configResourceKey]detectedConfigResource) {
	for key, resource := range src {
		dst[key] = resource
	}
}

func loadKubeconfigConfigMaps(ctx context.Context, client kubernetes.Interface, namespace string) (map[configResourceKey]detectedConfigResource, error) {
	configMaps, err := client.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	resources := map[configResourceKey]detectedConfigResource{}
	for _, configMap := range configMaps.Items {
		resource := detectKubeconfigsInConfigMap(configMap)
		if len(resource.KubeconfigKeys) == 0 {
			continue
		}
		resources[resource.key()] = resource
	}
	return resources, nil
}

func loadKubeconfigSecrets(ctx context.Context, client kubernetes.Interface, namespace string) (map[configResourceKey]detectedConfigResource, error) {
	secrets, err := client.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	resources := map[configResourceKey]detectedConfigResource{}
	for _, secret := range secrets.Items {
		resource := detectKubeconfigsInSecret(secret)
		if len(resource.KubeconfigKeys) == 0 {
			continue
		}
		resources[resource.key()] = resource
	}
	return resources, nil
}

func detectKubeconfigsInConfigMap(configMap corev1.ConfigMap) detectedConfigResource {
	resource := detectedConfigResource{
		Namespace: configMap.Namespace,
		Kind:      "ConfigMap",
		Name:      configMap.Name,
	}
	seen := map[string]struct{}{}

	for key, value := range configMap.Data {
		addKubeconfigKey(&resource, seen, key, []byte(value), "plain")
		addBase64DecodedKubeconfigKey(&resource, seen, key, []byte(value))
	}
	for key, value := range configMap.BinaryData {
		addKubeconfigKey(&resource, seen, key, value, "binary")
		addBase64DecodedKubeconfigKey(&resource, seen, key, value)
	}

	sortKubeconfigKeys(resource.KubeconfigKeys)
	return resource
}

func detectKubeconfigsInSecret(secret corev1.Secret) detectedConfigResource {
	resource := detectedConfigResource{
		Namespace: secret.Namespace,
		Kind:      "Secret",
		Name:      secret.Name,
	}
	seen := map[string]struct{}{}

	for key, value := range secret.Data {
		addKubeconfigKey(&resource, seen, key, value, "decoded")
		addBase64DecodedKubeconfigKey(&resource, seen, key, value)
	}
	for key, value := range secret.StringData {
		addKubeconfigKey(&resource, seen, key, []byte(value), "plain")
		addBase64DecodedKubeconfigKey(&resource, seen, key, []byte(value))
	}

	sortKubeconfigKeys(resource.KubeconfigKeys)
	return resource
}

func addKubeconfigKey(resource *detectedConfigResource, seen map[string]struct{}, key string, value []byte, encoding string) {
	if !isKubeconfig(value) {
		return
	}

	id := key + "\x00" + encoding
	if _, ok := seen[id]; ok {
		return
	}
	seen[id] = struct{}{}
	resource.KubeconfigKeys = append(resource.KubeconfigKeys, kubeconfigKeyFinding{
		Key:      key,
		Encoding: encoding,
	})
}

func addBase64DecodedKubeconfigKey(resource *detectedConfigResource, seen map[string]struct{}, key string, value []byte) {
	candidate := strings.Join(strings.Fields(strings.TrimSpace(string(value))), "")
	if candidate == "" {
		return
	}

	decoded, err := base64.StdEncoding.DecodeString(candidate)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(candidate)
	}
	if err != nil {
		return
	}

	addKubeconfigKey(resource, seen, key, decoded, "base64-decoded")
}

func isKubeconfig(value []byte) bool {
	value = bytes.TrimSpace(value)
	if len(value) == 0 {
		return false
	}

	config, err := clientcmd.Load(value)
	if err != nil || config == nil {
		return false
	}
	if len(config.Clusters) == 0 || len(config.Contexts) == 0 {
		return false
	}
	return config.CurrentContext != "" || len(config.AuthInfos) > 0
}

func collectPodConfigReferences(pod corev1.Pod) map[configResourceKey]podConfigReference {
	refs := map[configResourceKey]podConfigReference{}

	add := func(kind, name, source string) {
		if name == "" {
			return
		}
		key := configResourceKey{Namespace: pod.Namespace, Kind: kind, Name: name}
		ref := refs[key]
		if ref.sources == nil {
			ref = podConfigReference{
				key:     key,
				sources: map[string]struct{}{},
			}
		}
		ref.sources[source] = struct{}{}
		refs[key] = ref
	}

	for _, volume := range pod.Spec.Volumes {
		if volume.ConfigMap != nil {
			add("ConfigMap", volume.ConfigMap.Name, fmt.Sprintf("volume %q", volume.Name))
		}
		if volume.Secret != nil {
			add("Secret", volume.Secret.SecretName, fmt.Sprintf("volume %q", volume.Name))
		}
		if volume.Projected != nil {
			for _, source := range volume.Projected.Sources {
				if source.ConfigMap != nil {
					add("ConfigMap", source.ConfigMap.Name, fmt.Sprintf("projected volume %q", volume.Name))
				}
				if source.Secret != nil {
					add("Secret", source.Secret.Name, fmt.Sprintf("projected volume %q", volume.Name))
				}
			}
		}
	}

	for _, container := range pod.Spec.InitContainers {
		collectContainerConfigReferences(add, "initContainer", container.Name, container.Env, container.EnvFrom)
	}
	for _, container := range pod.Spec.Containers {
		collectContainerConfigReferences(add, "container", container.Name, container.Env, container.EnvFrom)
	}
	for _, container := range pod.Spec.EphemeralContainers {
		collectContainerConfigReferences(add, "ephemeralContainer", container.Name, container.Env, container.EnvFrom)
	}

	return refs
}

func collectContainerConfigReferences(add func(string, string, string), containerType, containerName string, env []corev1.EnvVar, envFrom []corev1.EnvFromSource) {
	containerLabel := fmt.Sprintf("%s %q", containerType, containerName)

	for _, source := range envFrom {
		if source.ConfigMapRef != nil {
			add("ConfigMap", source.ConfigMapRef.Name, containerLabel+" envFrom")
		}
		if source.SecretRef != nil {
			add("Secret", source.SecretRef.Name, containerLabel+" envFrom")
		}
	}

	for _, variable := range env {
		if variable.ValueFrom == nil {
			continue
		}
		if variable.ValueFrom.ConfigMapKeyRef != nil {
			add("ConfigMap", variable.ValueFrom.ConfigMapKeyRef.Name, fmt.Sprintf("%s env %q", containerLabel, variable.Name))
		}
		if variable.ValueFrom.SecretKeyRef != nil {
			add("Secret", variable.ValueFrom.SecretKeyRef.Name, fmt.Sprintf("%s env %q", containerLabel, variable.Name))
		}
	}
}

func loadOwnerResolver(ctx context.Context, client kubernetes.Interface, namespace string) (ownerResolver, []string) {
	resolver := ownerResolver{
		replicaSetOwners: map[types.NamespacedName]*metav1.OwnerReference{},
		jobOwners:        map[types.NamespacedName]*metav1.OwnerReference{},
	}
	var warnings []string

	replicaSets, err := client.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("could not list ReplicaSets; Deployment-owned pods may be reported as ReplicaSets: %v", err))
	} else {
		for _, replicaSet := range replicaSets.Items {
			resolver.replicaSetOwners[namespacedName(replicaSet.Namespace, replicaSet.Name)] = controllerOwner(&replicaSet.ObjectMeta)
		}
	}

	jobs, err := client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			warnings = append(warnings, fmt.Sprintf("could not list Jobs; CronJob-owned pods may be reported as Jobs: %v", err))
		}
	} else {
		for _, job := range jobs.Items {
			resolver.jobOwners[namespacedName(job.Namespace, job.Name)] = controllerOwner(&job.ObjectMeta)
		}
	}

	return resolver, warnings
}

func (resolver ownerResolver) resolvePodOwner(pod corev1.Pod) ownerKey {
	owner := controllerOwner(&pod.ObjectMeta)
	if owner == nil {
		return ownerKey{Namespace: pod.Namespace, Kind: "Pod", Name: pod.Name}
	}

	switch owner.Kind {
	case "ReplicaSet":
		if parent := resolver.replicaSetOwners[namespacedName(pod.Namespace, owner.Name)]; parent != nil && parent.Kind == "Deployment" {
			return ownerKey{Namespace: pod.Namespace, Kind: parent.Kind, Name: parent.Name}
		}
	case "Job":
		if parent := resolver.jobOwners[namespacedName(pod.Namespace, owner.Name)]; parent != nil && parent.Kind == "CronJob" {
			return ownerKey{Namespace: pod.Namespace, Kind: parent.Kind, Name: parent.Name}
		}
	}

	return ownerKey{Namespace: pod.Namespace, Kind: owner.Kind, Name: owner.Name}
}

func controllerOwner(meta *metav1.ObjectMeta) *metav1.OwnerReference {
	ref := metav1.GetControllerOf(meta)
	if ref == nil {
		return nil
	}
	copied := *ref
	return &copied
}

func flattenApplications(apps map[ownerKey]*appAccumulator, opts options) []applicationFinding {
	items := make([]applicationFinding, 0, len(apps))
	for _, app := range apps {
		podNames := sortedStringKeys(app.pods)
		item := applicationFinding{
			Namespace:       app.namespace,
			OwnerKind:       app.ownerKind,
			OwnerName:       app.ownerName,
			PodCount:        len(app.pods),
			SamplePods:      trimStrings(podNames, opts.maxSamples),
			ConfigResources: flattenConfigResourceAccumulators(app.resources),
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

func flattenConfigResourceAccumulators(resources map[configResourceKey]*configResourceAccumulator) []configResourceFinding {
	keys := sortedConfigResourceAccumulatorKeys(resources)
	items := make([]configResourceFinding, 0, len(keys))
	for _, key := range keys {
		items = append(items, resources[key].toFinding())
	}
	return items
}

func (resource detectedConfigResource) key() configResourceKey {
	return configResourceKey{
		Namespace: resource.Namespace,
		Kind:      resource.Kind,
		Name:      resource.Name,
	}
}

func (resource detectedConfigResource) toFinding(referenceSources map[string]struct{}) configResourceFinding {
	return configResourceFinding{
		Namespace:        resource.Namespace,
		Kind:             resource.Kind,
		Name:             resource.Name,
		KubeconfigKeys:   copyKubeconfigKeys(resource.KubeconfigKeys),
		ReferenceSources: sortedSet(referenceSources),
	}
}

func (resource *configResourceAccumulator) toFinding() configResourceFinding {
	return resource.resource.toFinding(resource.referenceSources)
}

func unreferencedConfigResources(resources map[configResourceKey]detectedConfigResource, referenced map[configResourceKey]struct{}) []configResourceFinding {
	keys := sortedDetectedConfigResourceKeys(resources)
	items := []configResourceFinding{}
	for _, key := range keys {
		if _, ok := referenced[key]; ok {
			continue
		}
		items = append(items, resources[key].toFinding(nil))
	}
	return items
}

func writeResult(w io.Writer, result scanResult, opts options) error {
	switch opts.output {
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

func writeTable(w io.Writer, result scanResult) error {
	if len(result.Items) == 0 {
		if _, err := fmt.Fprintln(w, "No applications referencing ConfigMaps or Secrets that contain kubeconfig were found."); err != nil {
			return err
		}
		if len(result.UnreferencedResources) > 0 {
			_, err := fmt.Fprintf(w, "Unreferenced kubeconfig resources: %d. Use --output json to inspect them.\n", len(result.UnreferencedResources))
			return err
		}
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAMESPACE\tOWNER\tCONFIG_RESOURCES\tPODS\tSAMPLE_PODS")
	for _, item := range result.Items {
		fmt.Fprintf(
			tw,
			"%s\t%s/%s\t%s\t%d\t%s\n",
			item.Namespace,
			item.OwnerKind,
			item.OwnerName,
			formatConfigResources(item.ConfigResources),
			item.PodCount,
			strings.Join(item.SamplePods, ","),
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(result.UnreferencedResources) > 0 {
		_, err := fmt.Fprintf(w, "\nUnreferenced kubeconfig resources: %d. Use --output json to inspect them.\n", len(result.UnreferencedResources))
		return err
	}
	return nil
}

func formatConfigResources(resources []configResourceFinding) string {
	parts := make([]string, 0, len(resources))
	for _, resource := range resources {
		parts = append(parts, fmt.Sprintf("%s/%s[%s]", resource.Kind, resource.Name, formatKubeconfigKeys(resource.KubeconfigKeys)))
	}
	return strings.Join(parts, "; ")
}

func formatKubeconfigKeys(keys []kubeconfigKeyFinding) string {
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s:%s", key.Key, key.Encoding))
	}
	return strings.Join(parts, ",")
}

func namespacedName(namespace, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: name}
}

func sortedStringKeys[V any](m map[string]V) []string {
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

func sortedConfigResourceAccumulatorKeys(m map[configResourceKey]*configResourceAccumulator) []configResourceKey {
	keys := make([]configResourceKey, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sortConfigResourceKeys(keys)
	return keys
}

func sortedDetectedConfigResourceKeys(m map[configResourceKey]detectedConfigResource) []configResourceKey {
	keys := make([]configResourceKey, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sortConfigResourceKeys(keys)
	return keys
}

func sortConfigResourceKeys(keys []configResourceKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Namespace != keys[j].Namespace {
			return keys[i].Namespace < keys[j].Namespace
		}
		if keys[i].Kind != keys[j].Kind {
			return keys[i].Kind < keys[j].Kind
		}
		return keys[i].Name < keys[j].Name
	})
}

func sortConfigResourceFindings(items []configResourceFinding) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Namespace != items[j].Namespace {
			return items[i].Namespace < items[j].Namespace
		}
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		return items[i].Name < items[j].Name
	})
}

func sortKubeconfigKeys(keys []kubeconfigKeyFinding) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Key != keys[j].Key {
			return keys[i].Key < keys[j].Key
		}
		return keys[i].Encoding < keys[j].Encoding
	})
}

func copyKubeconfigKeys(keys []kubeconfigKeyFinding) []kubeconfigKeyFinding {
	copied := append([]kubeconfigKeyFinding(nil), keys...)
	sortKubeconfigKeys(copied)
	return copied
}

func trimStrings(values []string, max int) []string {
	if len(values) <= max {
		return values
	}
	return append([]string(nil), values[:max]...)
}
