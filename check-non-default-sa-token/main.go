package main

import (
	"context"
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

const defaultServiceAccount = "default"

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
	Items    []applicationFinding `json:"items"`
	Warnings []string             `json:"warnings,omitempty"`
}

type applicationFinding struct {
	Namespace       string              `json:"namespace"`
	OwnerKind       string              `json:"ownerKind"`
	OwnerName       string              `json:"ownerName"`
	ServiceAccounts []string            `json:"serviceAccounts"`
	PodCount        int                 `json:"podCount"`
	SamplePods      []string            `json:"samplePods"`
	TokenSources    []string            `json:"tokenSources"`
	Pods            []podFindingDetails `json:"pods,omitempty"`
}

type podFindingDetails struct {
	Name            string   `json:"name"`
	ServiceAccounts []string `json:"serviceAccounts"`
	TokenSources    []string `json:"tokenSources"`
	Warnings        []string `json:"warnings,omitempty"`
}

type ownerKey struct {
	Namespace string
	Kind      string
	Name      string
}

type appAccumulator struct {
	namespace       string
	ownerKind       string
	ownerName       string
	serviceAccounts map[string]struct{}
	tokenSources    map[string]struct{}
	pods            map[string]podFindingDetails
}

type podTokenFinding struct {
	PodName         string
	ServiceAccounts map[string]struct{}
	TokenSources    map[string]struct{}
	Warnings        []string
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
	config.UserAgent = "check-non-default-sa-token"

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
	fs := flag.NewFlagSet("check-non-default-sa-token", flag.ContinueOnError)
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
	fs.BoolVar(&opts.skipSecretInspect, "skip-secret-inspection", false, "skip listing Secrets for legacy service-account-token volume attribution")
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

	pods, err := client.CoreV1().Pods(opts.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return result, fmt.Errorf("list pods: %w", err)
	}

	serviceAccounts, err := loadServiceAccountAutomounts(ctx, client, opts.namespace)
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("could not list ServiceAccounts; assuming automount defaults for pods without pod-level automountServiceAccountToken: %v", err))
	}

	secretTokenServiceAccounts := map[types.NamespacedName]string{}
	if !opts.skipSecretInspect {
		secretTokenServiceAccounts, err = loadServiceAccountTokenSecrets(ctx, client, opts.namespace)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("could not inspect Secrets; legacy service-account-token Secret volumes may be missed: %v", err))
		}
	}

	apps := map[ownerKey]*appAccumulator{}
	for _, pod := range pods.Items {
		podFinding := detectPodTokenUse(pod, serviceAccounts, secretTokenServiceAccounts)
		if len(podFinding.ServiceAccounts) == 0 {
			continue
		}

		owner := resolvePodOwner(pod)
		acc := apps[owner]
		if acc == nil {
			acc = &appAccumulator{
				namespace:       owner.Namespace,
				ownerKind:       owner.Kind,
				ownerName:       owner.Name,
				serviceAccounts: map[string]struct{}{},
				tokenSources:    map[string]struct{}{},
				pods:            map[string]podFindingDetails{},
			}
			apps[owner] = acc
		}
		for serviceAccount := range podFinding.ServiceAccounts {
			acc.serviceAccounts[serviceAccount] = struct{}{}
		}
		for source := range podFinding.TokenSources {
			acc.tokenSources[source] = struct{}{}
		}
		acc.pods[podFinding.PodName] = podFinding.toDetails()
	}

	result.Items = flattenApplications(apps, opts)
	return result, nil
}

func loadServiceAccountAutomounts(ctx context.Context, client kubernetes.Interface, namespace string) (map[types.NamespacedName]*bool, error) {
	serviceAccounts, err := client.CoreV1().ServiceAccounts(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	automounts := map[types.NamespacedName]*bool{}
	for _, serviceAccount := range serviceAccounts.Items {
		key := namespacedName(serviceAccount.Namespace, serviceAccount.Name)
		automounts[key] = serviceAccount.AutomountServiceAccountToken
	}
	return automounts, nil
}

func loadServiceAccountTokenSecrets(ctx context.Context, client kubernetes.Interface, namespace string) (map[types.NamespacedName]string, error) {
	secrets, err := client.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	tokenSecrets := map[types.NamespacedName]string{}
	for _, secret := range secrets.Items {
		if secret.Type != corev1.SecretTypeServiceAccountToken {
			continue
		}
		serviceAccount := secret.Annotations[corev1.ServiceAccountNameKey]
		if serviceAccount == "" {
			continue
		}
		tokenSecrets[namespacedName(secret.Namespace, secret.Name)] = serviceAccount
	}
	return tokenSecrets, nil
}

func detectPodTokenUse(pod corev1.Pod, serviceAccountAutomounts map[types.NamespacedName]*bool, secretTokenServiceAccounts map[types.NamespacedName]string) podTokenFinding {
	finding := podTokenFinding{
		PodName:         pod.Name,
		ServiceAccounts: map[string]struct{}{},
		TokenSources:    map[string]struct{}{},
	}

	podServiceAccount := pod.Spec.ServiceAccountName
	if podServiceAccount == "" {
		podServiceAccount = defaultServiceAccount
	}

	if podServiceAccount != defaultServiceAccount {
		if hasProjectedServiceAccountTokenVolume(pod) {
			finding.add(podServiceAccount, "projected serviceAccountToken volume")
		}
		if effectiveAutomount(pod, serviceAccountAutomounts) {
			finding.add(podServiceAccount, "effective automountServiceAccountToken=true")
		}
	}

	for _, volume := range pod.Spec.Volumes {
		if volume.Secret == nil || volume.Secret.SecretName == "" {
			continue
		}
		serviceAccount, ok := secretTokenServiceAccounts[namespacedName(pod.Namespace, volume.Secret.SecretName)]
		if !ok || serviceAccount == "" || serviceAccount == defaultServiceAccount {
			continue
		}
		source := fmt.Sprintf("legacy service-account-token Secret volume %q", volume.Secret.SecretName)
		finding.add(serviceAccount, source)
	}

	return finding
}

func (finding *podTokenFinding) add(serviceAccount, source string) {
	finding.ServiceAccounts[serviceAccount] = struct{}{}
	finding.TokenSources[source] = struct{}{}
}

func (finding podTokenFinding) toDetails() podFindingDetails {
	return podFindingDetails{
		Name:            finding.PodName,
		ServiceAccounts: sortedKeys(finding.ServiceAccounts),
		TokenSources:    sortedKeys(finding.TokenSources),
		Warnings:        append([]string(nil), finding.Warnings...),
	}
}

func effectiveAutomount(pod corev1.Pod, serviceAccountAutomounts map[types.NamespacedName]*bool) bool {
	if pod.Spec.AutomountServiceAccountToken != nil {
		return *pod.Spec.AutomountServiceAccountToken
	}

	serviceAccount := pod.Spec.ServiceAccountName
	if serviceAccount == "" {
		serviceAccount = defaultServiceAccount
	}

	if serviceAccountAutomounts != nil {
		if automount, ok := serviceAccountAutomounts[namespacedName(pod.Namespace, serviceAccount)]; ok && automount != nil {
			return *automount
		}
	}

	return true
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

func flattenApplications(apps map[ownerKey]*appAccumulator, opts options) []applicationFinding {
	items := make([]applicationFinding, 0, len(apps))
	for _, acc := range apps {
		podNames := sortedKeys(acc.pods)
		item := applicationFinding{
			Namespace:       acc.namespace,
			OwnerKind:       acc.ownerKind,
			OwnerName:       acc.ownerName,
			ServiceAccounts: sortedKeys(acc.serviceAccounts),
			PodCount:        len(acc.pods),
			SamplePods:      trimStrings(podNames, opts.maxSamples),
			TokenSources:    sortedKeys(acc.tokenSources),
		}
		if opts.includePodBreakdown {
			item.Pods = make([]podFindingDetails, 0, len(podNames))
			for _, podName := range podNames {
				item.Pods = append(item.Pods, acc.pods[podName])
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
		_, err := fmt.Fprintln(w, "No applications using non-default service account tokens were found.")
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAMESPACE\tOWNER\tSERVICE_ACCOUNTS\tPODS\tTOKEN_SOURCES\tSAMPLE_PODS")
	for _, item := range result.Items {
		fmt.Fprintf(
			tw,
			"%s\t%s/%s\t%s\t%d\t%s\t%s\n",
			item.Namespace,
			item.OwnerKind,
			item.OwnerName,
			strings.Join(item.ServiceAccounts, ","),
			item.PodCount,
			strings.Join(item.TokenSources, "; "),
			strings.Join(item.SamplePods, ","),
		)
	}
	return tw.Flush()
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

func trimStrings(values []string, max int) []string {
	if len(values) <= max {
		return values
	}
	return append([]string(nil), values[:max]...)
}
