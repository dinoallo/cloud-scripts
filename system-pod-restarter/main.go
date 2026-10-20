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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

const defaultExcludedNamespacePrefix = "ns-"

type options struct {
	kubeconfig              string
	contextName             string
	excludedNamespacePrefix string
	confirm                 bool
}

type podTarget struct {
	namespace string
	name      string
	ownerKind string
	ownerName string
}

type workloadRef struct {
	name string
	uid  types.UID
}

type replicaSetTarget struct {
	replicaSet workloadRef
	deployment workloadRef
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
	config.UserAgent = "system-pod-restarter"

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create kubernetes client: %w", err)
	}

	return runWithClient(ctx, client, opts, stdout, stderr)
}

func parseOptions(args []string, output io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet("system-pod-restarter", flag.ContinueOnError)
	fs.SetOutput(output)

	defaultKubeconfig := ""
	if home := homedir.HomeDir(); home != "" {
		defaultKubeconfig = filepath.Join(home, ".kube", "config")
	}

	fs.StringVar(&opts.kubeconfig, "kubeconfig", defaultKubeconfig, "path to kubeconfig; falls back to in-cluster config when unavailable")
	fs.StringVar(&opts.contextName, "context", "", "kubeconfig context to use")
	fs.StringVar(&opts.excludedNamespacePrefix, "exclude-namespace-prefix", defaultExcludedNamespacePrefix, "skip namespaces whose names start with this prefix")
	fs.BoolVar(&opts.confirm, "confirm", false, "delete matching Pods; without this flag the program only prints a dry-run plan")

	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() > 0 {
		return opts, fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}
	if opts.excludedNamespacePrefix == "" {
		return opts, errors.New("--exclude-namespace-prefix must not be empty")
	}
	return opts, nil
}

func runWithClient(ctx context.Context, client kubernetes.Interface, opts options, stdout, stderr io.Writer) error {
	targets, err := findRestartTargets(ctx, client, opts.excludedNamespacePrefix)
	if err != nil {
		return err
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

func findRestartTargets(ctx context.Context, client kubernetes.Interface, excludedNamespacePrefix string) ([]podTarget, error) {
	namespaces, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}

	sort.Slice(namespaces.Items, func(i, j int) bool {
		return namespaces.Items[i].Name < namespaces.Items[j].Name
	})

	var targets []podTarget
	for _, namespace := range namespaces.Items {
		if strings.HasPrefix(namespace.Name, excludedNamespacePrefix) {
			continue
		}

		namespaceTargets, err := findNamespaceTargets(ctx, client, namespace.Name)
		if err != nil {
			return nil, err
		}
		targets = append(targets, namespaceTargets...)
	}

	sort.Slice(targets, func(i, j int) bool {
		if targets[i].namespace != targets[j].namespace {
			return targets[i].namespace < targets[j].namespace
		}
		if targets[i].name != targets[j].name {
			return targets[i].name < targets[j].name
		}
		if targets[i].ownerKind != targets[j].ownerKind {
			return targets[i].ownerKind < targets[j].ownerKind
		}
		return targets[i].ownerName < targets[j].ownerName
	})
	return targets, nil
}

func findNamespaceTargets(ctx context.Context, client kubernetes.Interface, namespace string) ([]podTarget, error) {
	deployments, err := client.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list Deployments in namespace %q: %w", namespace, err)
	}
	daemonSets, err := client.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list DaemonSets in namespace %q: %w", namespace, err)
	}
	if len(deployments.Items) == 0 && len(daemonSets.Items) == 0 {
		return nil, nil
	}

	replicaSets, err := client.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list ReplicaSets in namespace %q: %w", namespace, err)
	}
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list Pods in namespace %q: %w", namespace, err)
	}

	deploymentRefs := make(map[string]workloadRef, len(deployments.Items))
	for _, deployment := range deployments.Items {
		deploymentRefs[deployment.Name] = workloadRef{name: deployment.Name, uid: deployment.UID}
	}
	daemonSetRefs := make(map[string]workloadRef, len(daemonSets.Items))
	for _, daemonSet := range daemonSets.Items {
		daemonSetRefs[daemonSet.Name] = workloadRef{name: daemonSet.Name, uid: daemonSet.UID}
	}

	replicaSetTargets := make(map[string]replicaSetTarget)
	for _, replicaSet := range replicaSets.Items {
		owner := metav1.GetControllerOf(&replicaSet)
		if owner == nil || owner.Kind != "Deployment" {
			continue
		}
		deployment, ok := deploymentRefs[owner.Name]
		if !ok || !ownerMatches(owner, deployment) {
			continue
		}
		replicaSetTargets[replicaSet.Name] = replicaSetTarget{
			replicaSet: workloadRef{name: replicaSet.Name, uid: replicaSet.UID},
			deployment: deployment,
		}
	}

	targetsByPod := make(map[string]podTarget)
	for _, pod := range pods.Items {
		owner := metav1.GetControllerOf(&pod)
		if owner == nil {
			continue
		}

		switch owner.Kind {
		case "DaemonSet":
			daemonSet, ok := daemonSetRefs[owner.Name]
			if ok && ownerMatches(owner, daemonSet) {
				addPodTarget(targetsByPod, pod.Name, podTarget{
					namespace: namespace,
					name:      pod.Name,
					ownerKind: "DaemonSet",
					ownerName: daemonSet.name,
				})
			}
		case "Deployment":
			deployment, ok := deploymentRefs[owner.Name]
			if ok && ownerMatches(owner, deployment) {
				addPodTarget(targetsByPod, pod.Name, podTarget{
					namespace: namespace,
					name:      pod.Name,
					ownerKind: "Deployment",
					ownerName: deployment.name,
				})
			}
		case "ReplicaSet":
			replicaSet, ok := replicaSetTargets[owner.Name]
			if ok && ownerMatches(owner, replicaSet.replicaSet) {
				addPodTarget(targetsByPod, pod.Name, podTarget{
					namespace: namespace,
					name:      pod.Name,
					ownerKind: "Deployment",
					ownerName: replicaSet.deployment.name,
				})
			}
		}
	}

	targets := make([]podTarget, 0, len(targetsByPod))
	for _, target := range targetsByPod {
		targets = append(targets, target)
	}
	return targets, nil
}

func addPodTarget(targets map[string]podTarget, podName string, target podTarget) {
	if _, exists := targets[podName]; !exists {
		targets[podName] = target
	}
}

func ownerMatches(owner *metav1.OwnerReference, ref workloadRef) bool {
	if owner == nil || owner.Name != ref.name {
		return false
	}
	if ref.uid != "" && owner.UID != "" && ref.uid != owner.UID {
		return false
	}
	return true
}

func deleteTargets(ctx context.Context, client kubernetes.Interface, targets []podTarget, stdout, stderr io.Writer) error {
	var errs []error
	for _, target := range targets {
		err := client.CoreV1().Pods(target.namespace).Delete(ctx, target.name, metav1.DeleteOptions{})
		if apierrors.IsNotFound(err) {
			printTarget(stdout, target, "already-absent")
			continue
		}
		if err != nil {
			wrapped := fmt.Errorf("delete Pod %s/%s: %w", target.namespace, target.name, err)
			fmt.Fprintf(stderr, "error: %v\n", wrapped)
			errs = append(errs, wrapped)
			continue
		}
		printTarget(stdout, target, "deleted")
	}
	return errors.Join(errs...)
}

func printTarget(output io.Writer, target podTarget, status string) {
	fmt.Fprintf(output, "- pod=%s/%s owner=%s/%s status=%s\n",
		target.namespace,
		target.name,
		target.ownerKind,
		target.ownerName,
		status,
	)
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
