package main

import (
	"bytes"
	"context"
	"encoding/base64"
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
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

const (
	appName               = "check-apiserver-access-candidates"
	defaultServiceAccount = "default"
	confidenceLikely      = "likely"
	confidencePossible    = "possible"
)

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
	Confidence      string                  `json:"confidence"`
	ServiceAccounts []string                `json:"serviceAccounts"`
	Reasons         []string                `json:"reasons"`
	PodCount        int                     `json:"podCount"`
	SamplePods      []string                `json:"samplePods"`
	ConfigResources []configResourceFinding `json:"configResources,omitempty"`
	Pods            []podFindingDetails     `json:"pods,omitempty"`
}

type podFindingDetails struct {
	Name            string                  `json:"name"`
	ServiceAccount  string                  `json:"serviceAccount"`
	Confidence      string                  `json:"confidence"`
	Reasons         []string                `json:"reasons"`
	ConfigResources []configResourceFinding `json:"configResources,omitempty"`
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

type serviceAccountKey struct {
	Namespace string
	Name      string
}

type serviceAccountRisk struct {
	HasRBAC     bool
	BindingRefs map[string]struct{}
	Automount   *bool
	Exists      bool
	Loaded      bool
}

type appAccumulator struct {
	namespace       string
	ownerKind       string
	ownerName       string
	confidence      string
	serviceAccounts map[string]struct{}
	reasons         map[string]struct{}
	pods            map[string]podFindingDetails
	resources       map[configResourceKey]*configResourceAccumulator
}

type configResourceAccumulator struct {
	resource         detectedConfigResource
	referenceSources map[string]struct{}
}

type podConfigReference struct {
	key     configResourceKey
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
	fs.BoolVar(&opts.skipSecretInspect, "skip-secret-inspection", false, "skip listing Secrets for kubeconfig detection")
	fs.BoolVar(&opts.includePodBreakdown, "include-pods", false, "include per-Pod details in JSON output")

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

	pods, err := client.CoreV1().Pods(opts.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return result, fmt.Errorf("list pods: %w", err)
	}

	serviceAccountRisks, err := loadServiceAccountRisks(ctx, client, opts.namespace)
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("could not inspect ServiceAccounts or RBAC bindings; RBAC-backed candidates may be incomplete: %v", err))
	}

	configResources := map[configResourceKey]detectedConfigResource{}
	configMaps, err := loadKubeconfigConfigMaps(ctx, client, opts.namespace)
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("could not inspect ConfigMaps; kubeconfig candidates may be missed: %v", err))
	} else {
		mergeConfigResources(configResources, configMaps)
	}

	if !opts.skipSecretInspect {
		secrets, err := loadKubeconfigSecrets(ctx, client, opts.namespace)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("could not inspect Secrets; Secret kubeconfig candidates may be missed: %v", err))
		} else {
			mergeConfigResources(configResources, secrets)
		}
	}

	apps := map[ownerKey]*appAccumulator{}
	referencedResources := map[configResourceKey]struct{}{}

	for _, pod := range pods.Items {
		podFinding := detectPodAPIServerAccessCandidate(pod, serviceAccountRisks, configResources)
		if podFinding.Confidence == "" {
			continue
		}

		owner := resolvePodOwner(pod)
		app := apps[owner]
		if app == nil {
			app = &appAccumulator{
				namespace:       owner.Namespace,
				ownerKind:       owner.Kind,
				ownerName:       owner.Name,
				serviceAccounts: map[string]struct{}{},
				reasons:         map[string]struct{}{},
				pods:            map[string]podFindingDetails{},
				resources:       map[configResourceKey]*configResourceAccumulator{},
			}
			apps[owner] = app
		}

		app.confidence = maxConfidence(app.confidence, podFinding.Confidence)
		app.serviceAccounts[podFinding.ServiceAccount] = struct{}{}
		for _, reason := range podFinding.Reasons {
			app.reasons[reason] = struct{}{}
		}
		for _, resource := range podFinding.ConfigResources {
			key := configResourceKey{Namespace: resource.Namespace, Kind: resource.Kind, Name: resource.Name}
			referencedResources[key] = struct{}{}
			resourceAcc := app.resources[key]
			if resourceAcc == nil {
				resourceAcc = &configResourceAccumulator{
					resource: detectedConfigResource{
						Namespace:      resource.Namespace,
						Kind:           resource.Kind,
						Name:           resource.Name,
						KubeconfigKeys: resource.KubeconfigKeys,
					},
					referenceSources: map[string]struct{}{},
				}
				app.resources[key] = resourceAcc
			}
			for _, source := range resource.ReferenceSources {
				resourceAcc.referenceSources[source] = struct{}{}
			}
		}
		app.pods[podFinding.Name] = podFinding
	}

	result.Items = flattenApplications(apps, opts)
	result.UnreferencedResources = unreferencedConfigResources(configResources, referencedResources)
	return result, nil
}

func loadServiceAccountRisks(ctx context.Context, client kubernetes.Interface, namespace string) (map[serviceAccountKey]serviceAccountRisk, error) {
	serviceAccounts, err := client.CoreV1().ServiceAccounts(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	risks := map[serviceAccountKey]serviceAccountRisk{}
	for _, serviceAccount := range serviceAccounts.Items {
		key := serviceAccountKey{Namespace: serviceAccount.Namespace, Name: serviceAccount.Name}
		risk := risks[key]
		risk.Automount = serviceAccount.AutomountServiceAccountToken
		risk.Exists = true
		risk.Loaded = true
		if risk.BindingRefs == nil {
			risk.BindingRefs = map[string]struct{}{}
		}
		risks[key] = risk
	}

	roleBindings, err := client.RbacV1().RoleBindings(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, binding := range roleBindings.Items {
		addBindingSubjects(risks, binding.Namespace, binding.Subjects, "RoleBinding/"+binding.Namespace+"/"+binding.Name)
	}

	clusterRoleBindings, err := client.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, binding := range clusterRoleBindings.Items {
		addBindingSubjects(risks, metav1.NamespaceAll, binding.Subjects, "ClusterRoleBinding/"+binding.Name)
	}

	return risks, nil
}

func addBindingSubjects(risks map[serviceAccountKey]serviceAccountRisk, bindingNamespace string, subjects []rbacv1.Subject, bindingRef string) {
	for _, subject := range subjects {
		if subject.Kind != rbacv1.ServiceAccountKind {
			continue
		}
		namespace := subject.Namespace
		if namespace == "" {
			namespace = bindingNamespace
		}
		if namespace == "" || subject.Name == "" {
			continue
		}
		key := serviceAccountKey{Namespace: namespace, Name: subject.Name}
		risk := risks[key]
		risk.HasRBAC = true
		if risk.BindingRefs == nil {
			risk.BindingRefs = map[string]struct{}{}
		}
		risk.BindingRefs[bindingRef] = struct{}{}
		risks[key] = risk
	}
}

func detectPodAPIServerAccessCandidate(pod corev1.Pod, serviceAccountRisks map[serviceAccountKey]serviceAccountRisk, configResources map[configResourceKey]detectedConfigResource) podFindingDetails {
	finding := podFindingDetails{
		Name:           pod.Name,
		ServiceAccount: podServiceAccount(pod),
	}
	reasons := map[string]struct{}{}

	refs := collectPodConfigReferences(pod)
	for _, key := range sortedConfigReferenceKeys(refs) {
		resource, ok := configResources[key]
		if !ok {
			continue
		}
		finding.ConfigResources = append(finding.ConfigResources, resource.toFinding(refs[key].sources))
		reasons["references kubeconfig "+resource.Kind+"/"+resource.Name] = struct{}{}
	}
	if len(finding.ConfigResources) > 0 {
		finding.Confidence = confidenceLikely
	}

	if hasProjectedServiceAccountTokenVolume(pod) {
		reasons["has explicit projected serviceAccountToken volume"] = struct{}{}
		finding.Confidence = maxConfidence(finding.Confidence, confidenceLikely)
	}

	serviceAccount := serviceAccountKey{Namespace: pod.Namespace, Name: podServiceAccount(pod)}
	risk := serviceAccountRisks[serviceAccount]
	if risk.HasRBAC {
		reasons["serviceAccount has RBAC binding"] = struct{}{}
		for _, bindingRef := range sortedSet(risk.BindingRefs) {
			reasons["bound by "+bindingRef] = struct{}{}
		}
		if effectiveAutomount(pod, risk) {
			reasons["effective automountServiceAccountToken=true"] = struct{}{}
			finding.Confidence = maxConfidence(finding.Confidence, confidenceLikely)
		}
	}

	if finding.Confidence == "" && effectiveAutomount(pod, risk) {
		reasons["effective automountServiceAccountToken=true"] = struct{}{}
		finding.Confidence = confidencePossible
	}

	finding.Reasons = sortedSet(reasons)
	sortConfigResourceFindings(finding.ConfigResources)
	return finding
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

func hasProjectedServiceAccountTokenVolume(pod corev1.Pod) bool {
	for _, volume := range pod.Spec.Volumes {
		if volume.Projected == nil {
			continue
		}
		for _, source := range volume.Projected.Sources {
			if source.ServiceAccountToken != nil {
				return true
			}
		}
	}
	return false
}

func effectiveAutomount(pod corev1.Pod, risk serviceAccountRisk) bool {
	if pod.Spec.AutomountServiceAccountToken != nil {
		return *pod.Spec.AutomountServiceAccountToken
	}
	if risk.Automount != nil {
		return *risk.Automount
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

func flattenApplications(apps map[ownerKey]*appAccumulator, opts options) []applicationFinding {
	items := make([]applicationFinding, 0, len(apps))
	for _, app := range apps {
		podNames := sortedKeys(app.pods)
		item := applicationFinding{
			Namespace:       app.namespace,
			OwnerKind:       app.ownerKind,
			OwnerName:       app.ownerName,
			Confidence:      app.confidence,
			ServiceAccounts: sortedSet(app.serviceAccounts),
			Reasons:         sortedSet(app.reasons),
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
		if confidenceRank(items[i].Confidence) != confidenceRank(items[j].Confidence) {
			return confidenceRank(items[i].Confidence) > confidenceRank(items[j].Confidence)
		}
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

func unreferencedConfigResources(configResources map[configResourceKey]detectedConfigResource, referenced map[configResourceKey]struct{}) []configResourceFinding {
	keys := sortedConfigResourceKeys(configResources)
	items := []configResourceFinding{}
	for _, key := range keys {
		if _, ok := referenced[key]; ok {
			continue
		}
		items = append(items, configResources[key].toFinding(nil))
	}
	return items
}

func (resource detectedConfigResource) key() configResourceKey {
	return configResourceKey{Namespace: resource.Namespace, Kind: resource.Kind, Name: resource.Name}
}

func (resource detectedConfigResource) toFinding(referenceSources map[string]struct{}) configResourceFinding {
	return configResourceFinding{
		Namespace:        resource.Namespace,
		Kind:             resource.Kind,
		Name:             resource.Name,
		KubeconfigKeys:   append([]kubeconfigKeyFinding(nil), resource.KubeconfigKeys...),
		ReferenceSources: sortedSet(referenceSources),
	}
}

func (resource *configResourceAccumulator) toFinding() configResourceFinding {
	return resource.resource.toFinding(resource.referenceSources)
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
	if err := writer.Write([]string{"namespace", "ownerKind", "ownerName", "confidence", "serviceAccounts", "reasons", "pods"}); err != nil {
		return err
	}
	for _, item := range result.Items {
		if err := writer.Write([]string{
			item.Namespace,
			item.OwnerKind,
			item.OwnerName,
			item.Confidence,
			strings.Join(item.ServiceAccounts, ","),
			strings.Join(item.Reasons, "; "),
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
		if _, err := fmt.Fprintln(w, "No apiserver access candidates were found."); err != nil {
			return err
		}
		if len(result.UnreferencedResources) > 0 {
			_, err := fmt.Fprintf(w, "Unreferenced kubeconfig resources: %d. Use --output json to inspect them.\n", len(result.UnreferencedResources))
			return err
		}
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAMESPACE\tOWNER\tCONFIDENCE\tSERVICE_ACCOUNTS\tPODS\tREASONS\tSAMPLE_PODS")
	for _, item := range result.Items {
		fmt.Fprintf(
			tw,
			"%s\t%s/%s\t%s\t%s\t%d\t%s\t%s\n",
			item.Namespace,
			item.OwnerKind,
			item.OwnerName,
			item.Confidence,
			strings.Join(item.ServiceAccounts, ","),
			item.PodCount,
			strings.Join(item.Reasons, "; "),
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

func maxConfidence(a, b string) string {
	if confidenceRank(b) > confidenceRank(a) {
		return b
	}
	return a
}

func confidenceRank(confidence string) int {
	switch confidence {
	case confidenceLikely:
		return 2
	case confidencePossible:
		return 1
	default:
		return 0
	}
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

func sortedConfigResourceKeys(resources map[configResourceKey]detectedConfigResource) []configResourceKey {
	keys := make([]configResourceKey, 0, len(resources))
	for key := range resources {
		keys = append(keys, key)
	}
	sortConfigResourceKeys(keys)
	return keys
}

func sortedConfigResourceAccumulatorKeys(resources map[configResourceKey]*configResourceAccumulator) []configResourceKey {
	keys := make([]configResourceKey, 0, len(resources))
	for key := range resources {
		keys = append(keys, key)
	}
	sortConfigResourceKeys(keys)
	return keys
}

func sortedConfigReferenceKeys(refs map[configResourceKey]podConfigReference) []configResourceKey {
	keys := make([]configResourceKey, 0, len(refs))
	for key := range refs {
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

func trimStrings(values []string, max int) []string {
	if len(values) <= max {
		return values
	}
	return append([]string(nil), values[:max]...)
}

func namespacedName(namespace, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: name}
}
