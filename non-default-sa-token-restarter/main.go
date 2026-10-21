package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

const (
	appName               = "non-default-sa-token-restarter"
	defaultServiceAccount = "default"
)

type options struct {
	kubeconfig           string
	contextName          string
	namespace            string
	node                 string
	excludePod           string
	confirm              bool
	skipSecretInspection bool
}

type podTarget struct {
	namespace       string
	name            string
	node            string
	ownerKind       string
	ownerName       string
	serviceAccounts []string
	tokenSources    []string
}

type podTokenFinding struct {
	serviceAccounts map[string]struct{}
	tokenSources    map[string]struct{}
}

type podSecretReference struct {
	name   string
	source string
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

	return runWithClient(ctx, client, opts, stdout, stderr)
}

func parseOptions(args []string, output io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet(appName, flag.ContinueOnError)
	fs.SetOutput(output)

	defaultKubeconfig := ""
	if home := homedir.HomeDir(); home != "" {
		defaultKubeconfig = filepath.Join(home, ".kube", "config")
	}
	defaultExcludedPod := excludedPodFromEnvironment()

	fs.StringVar(&opts.kubeconfig, "kubeconfig", defaultKubeconfig, "path to kubeconfig; falls back to in-cluster config when unavailable")
	fs.StringVar(&opts.contextName, "context", "", "kubeconfig context to use")
	fs.StringVar(&opts.namespace, "namespace", metav1.NamespaceAll, "namespace to scan; empty means all namespaces")
	fs.StringVar(&opts.node, "node", "", "only scan Pods scheduled on this node; empty means all nodes")
	fs.StringVar(&opts.excludePod, "exclude-pod", defaultExcludedPod, "exclude one Pod in namespace/name form; defaults to the Pod identified by POD_NAMESPACE and POD_NAME")
	fs.BoolVar(&opts.confirm, "confirm", false, "delete matching Pods; without this flag the program only prints a dry-run plan")
	fs.BoolVar(&opts.skipSecretInspection, "skip-secret-inspection", false, "skip listing Secrets; legacy ServiceAccount-token Secret references may be missed")

	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() > 0 {
		return opts, fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}
	if opts.excludePod != "" {
		if _, _, err := splitPodReference(opts.excludePod); err != nil {
			return opts, fmt.Errorf("invalid --exclude-pod: %w", err)
		}
	}
	return opts, nil
}

func excludedPodFromEnvironment() string {
	namespace := strings.TrimSpace(os.Getenv("POD_NAMESPACE"))
	name := strings.TrimSpace(os.Getenv("POD_NAME"))
	if namespace == "" || name == "" {
		return ""
	}
	return namespace + "/" + name
}

func runWithClient(ctx context.Context, client kubernetes.Interface, opts options, stdout, stderr io.Writer) error {
	targets, warnings, err := findMatchingPods(ctx, client, opts)
	if err != nil {
		return err
	}
	for _, warning := range warnings {
		fmt.Fprintf(stderr, "warning: %s\n", warning)
	}

	if !opts.confirm {
		if len(targets) == 0 {
			fmt.Fprintln(stdout, "dry-run: no matching Pods found")
			return nil
		}
		fmt.Fprintf(stdout, "dry-run: found %d matching Pod(s); pass --confirm to delete\n", len(targets))
		for _, target := range targets {
			printTarget(stdout, target, "would-delete")
		}
		return nil
	}

	if len(targets) == 0 {
		fmt.Fprintln(stdout, "no matching Pods found")
		return nil
	}

	fmt.Fprintf(stdout, "deleting %d matching Pod(s)\n", len(targets))
	return deleteTargets(ctx, client, targets, stdout, stderr)
}

func findMatchingPods(ctx context.Context, client kubernetes.Interface, opts options) ([]podTarget, []string, error) {
	serviceAccountAutomounts, err := loadServiceAccountAutomounts(ctx, client, opts.namespace)
	if err != nil {
		return nil, nil, fmt.Errorf("list ServiceAccounts: %w", err)
	}

	secretTokenServiceAccounts := map[types.NamespacedName]string{}
	var warnings []string
	if !opts.skipSecretInspection {
		secretTokenServiceAccounts, err = loadServiceAccountTokenSecrets(ctx, client, opts.namespace)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("could not inspect Secrets; legacy ServiceAccount-token Secret references may be missed: %v", err))
		}
	}

	listOptions := metav1.ListOptions{}
	if opts.node != "" {
		listOptions.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", opts.node).String()
	}
	pods, err := client.CoreV1().Pods(opts.namespace).List(ctx, listOptions)
	if err != nil {
		return nil, warnings, fmt.Errorf("list Pods: %w", err)
	}

	excludedNamespace, excludedName, err := splitPodReference(opts.excludePod)
	if err != nil {
		return nil, warnings, fmt.Errorf("parse excluded Pod: %w", err)
	}

	targets := make([]podTarget, 0)
	for _, pod := range pods.Items {
		if pod.Spec.NodeName == "" || (opts.node != "" && pod.Spec.NodeName != opts.node) {
			continue
		}
		if pod.Namespace == excludedNamespace && pod.Name == excludedName {
			continue
		}
		if pod.DeletionTimestamp != nil || isTerminalPod(pod) {
			continue
		}

		finding := detectPodTokenUse(pod, serviceAccountAutomounts, secretTokenServiceAccounts)
		if len(finding.serviceAccounts) == 0 {
			continue
		}

		ownerKind, ownerName := podOwner(pod)
		targets = append(targets, podTarget{
			namespace:       pod.Namespace,
			name:            pod.Name,
			node:            pod.Spec.NodeName,
			ownerKind:       ownerKind,
			ownerName:       ownerName,
			serviceAccounts: sortedKeys(finding.serviceAccounts),
			tokenSources:    sortedKeys(finding.tokenSources),
		})
	}

	sort.Slice(targets, func(i, j int) bool {
		if targets[i].namespace != targets[j].namespace {
			return targets[i].namespace < targets[j].namespace
		}
		return targets[i].name < targets[j].name
	})
	return targets, warnings, nil
}

func isTerminalPod(pod corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

func loadServiceAccountAutomounts(ctx context.Context, client kubernetes.Interface, namespace string) (map[types.NamespacedName]*bool, error) {
	serviceAccounts, err := client.CoreV1().ServiceAccounts(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	automounts := make(map[types.NamespacedName]*bool, len(serviceAccounts.Items))
	for _, serviceAccount := range serviceAccounts.Items {
		automounts[namespacedName(serviceAccount.Namespace, serviceAccount.Name)] = serviceAccount.AutomountServiceAccountToken
	}
	return automounts, nil
}

func loadServiceAccountTokenSecrets(ctx context.Context, client kubernetes.Interface, namespace string) (map[types.NamespacedName]string, error) {
	secrets, err := client.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	tokenSecrets := make(map[types.NamespacedName]string)
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
		serviceAccounts: map[string]struct{}{},
		tokenSources:    map[string]struct{}{},
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

	for _, reference := range podSecretReferences(pod) {
		serviceAccount, ok := secretTokenServiceAccounts[namespacedName(pod.Namespace, reference.name)]
		if !ok || serviceAccount == "" || serviceAccount == defaultServiceAccount {
			continue
		}
		finding.add(serviceAccount, reference.source)
	}

	return finding
}

func (finding *podTokenFinding) add(serviceAccount, source string) {
	finding.serviceAccounts[serviceAccount] = struct{}{}
	finding.tokenSources[source] = struct{}{}
}

func effectiveAutomount(pod corev1.Pod, serviceAccountAutomounts map[types.NamespacedName]*bool) bool {
	if pod.Spec.AutomountServiceAccountToken != nil {
		return *pod.Spec.AutomountServiceAccountToken
	}

	serviceAccount := pod.Spec.ServiceAccountName
	if serviceAccount == "" {
		serviceAccount = defaultServiceAccount
	}

	if automount, ok := serviceAccountAutomounts[namespacedName(pod.Namespace, serviceAccount)]; ok && automount != nil {
		return *automount
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

func podSecretReferences(pod corev1.Pod) []podSecretReference {
	var references []podSecretReference
	for _, volume := range pod.Spec.Volumes {
		if volume.Secret != nil && volume.Secret.SecretName != "" {
			references = append(references, podSecretReference{
				name:   volume.Secret.SecretName,
				source: fmt.Sprintf("legacy service-account-token Secret volume %q", volume.Secret.SecretName),
			})
		}
		if volume.Projected == nil {
			continue
		}
		for _, source := range volume.Projected.Sources {
			if source.Secret != nil && source.Secret.Name != "" {
				references = append(references, podSecretReference{
					name:   source.Secret.Name,
					source: fmt.Sprintf("projected Secret volume %q", source.Secret.Name),
				})
			}
		}
	}

	for _, container := range pod.Spec.InitContainers {
		references = append(references, containerSecretReferences("init container "+container.Name, container.Env, container.EnvFrom)...)
	}
	for _, container := range pod.Spec.Containers {
		references = append(references, containerSecretReferences("container "+container.Name, container.Env, container.EnvFrom)...)
	}
	for _, container := range pod.Spec.EphemeralContainers {
		references = append(references, containerSecretReferences("ephemeral container "+container.Name, container.Env, container.EnvFrom)...)
	}
	return references
}

func containerSecretReferences(container string, env []corev1.EnvVar, envFrom []corev1.EnvFromSource) []podSecretReference {
	var references []podSecretReference
	for _, variable := range env {
		if variable.ValueFrom == nil || variable.ValueFrom.SecretKeyRef == nil || variable.ValueFrom.SecretKeyRef.Name == "" {
			continue
		}
		references = append(references, podSecretReference{
			name:   variable.ValueFrom.SecretKeyRef.Name,
			source: fmt.Sprintf("%s SecretKeyRef for env %q", container, variable.Name),
		})
	}
	for _, source := range envFrom {
		if source.SecretRef == nil || source.SecretRef.Name == "" {
			continue
		}
		references = append(references, podSecretReference{
			name:   source.SecretRef.Name,
			source: fmt.Sprintf("%s SecretRef for envFrom", container),
		})
	}
	return references
}

func podOwner(pod corev1.Pod) (string, string) {
	owner := metav1.GetControllerOf(&pod)
	if owner == nil {
		return "Pod", pod.Name
	}
	return owner.Kind, owner.Name
}

func deleteTargets(ctx context.Context, client kubernetes.Interface, targets []podTarget, stdout, stderr io.Writer) error {
	var errs []error
	for _, target := range targets {
		err := client.CoreV1().Pods(target.namespace).Delete(ctx, target.name, metav1.DeleteOptions{})
		if err != nil {
			printErr := fmt.Errorf("delete Pod %s/%s: %w", target.namespace, target.name, err)
			if apierrors.IsNotFound(err) {
				printTarget(stdout, target, "already-absent")
				continue
			}
			fmt.Fprintf(stderr, "error: %v\n", printErr)
			errs = append(errs, printErr)
			continue
		}
		printTarget(stdout, target, "deleted")
	}
	return errors.Join(errs...)
}

func printTarget(output io.Writer, target podTarget, status string) {
	owner := target.ownerKind + "/" + target.ownerName
	node := target.node
	if node == "" {
		node = "<unassigned>"
	}
	fmt.Fprintf(output, "- pod=%s/%s node=%s owner=%s serviceAccounts=%s tokenSources=%s status=%s\n",
		target.namespace,
		target.name,
		node,
		owner,
		strings.Join(target.serviceAccounts, ","),
		strings.Join(target.tokenSources, "; "),
		status,
	)
}

func splitPodReference(reference string) (string, string, error) {
	if reference == "" {
		return "", "", nil
	}
	parts := strings.Split(reference, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected namespace/name, got %q", reference)
	}
	return parts[0], parts[1], nil
}

func namespacedName(namespace, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: name}
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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
