package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	templateDeployKey  = "cloud.sealos.io/deploy-on-sealos"
	appDeployKey       = "cloud.sealos.io/app-deploy-manager"
	legacyAppLabelKey  = "app"
	pathAnnotationKey  = "path"
	valueAnnotationKey = "value"
)

type stringList []string

func (s *stringList) String() string {
	return strings.Join(*s, ",")
}

func (s *stringList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	*s = append(*s, value)
	return nil
}

type options struct {
	kubeconfig      string
	namespace       string
	instance        string
	statefulSets    stringList
	claimTemplates  stringList
	confirm         bool
	deletePV        bool
	allowNameOnly   bool
	discoverOrphans bool
	wait            time.Duration
}

type targetPVC struct {
	namespace         string
	name              string
	path              string
	value             string
	reason            string
	pvc               *corev1.PersistentVolumeClaim
	pv                *corev1.PersistentVolume
	pvClaimRefMatched bool
}

func main() {
	if err := run(context.Background(), parseFlags()); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags() options {
	var opts options
	flag.StringVar(&opts.kubeconfig, "kubeconfig", "", "path to kubeconfig")
	flag.StringVar(&opts.namespace, "namespace", "", "namespace containing leftover template PVCs")
	flag.StringVar(&opts.instance, "instance", "", "deprecated; PVC-only cleanup does not support template instance scope")
	flag.Var(&opts.statefulSets, "statefulset", "deprecated; PVC-only cleanup does not inspect StatefulSets")
	flag.Var(&opts.claimTemplates, "claim-template", "deprecated; PVC-only cleanup does not inspect volumeClaimTemplates")
	flag.BoolVar(&opts.confirm, "confirm", false, "delete matching PVCs/PVs; without this flag the program only prints a dry-run plan")
	flag.BoolVar(&opts.deletePV, "delete-pv", true, "delete a leftover PV after its target PVC is gone and the PV claimRef still matches")
	flag.BoolVar(&opts.allowNameOnly, "allow-name-only", false, "deprecated; PVC-only cleanup does not support StatefulSet name-only matching")
	flag.BoolVar(&opts.discoverOrphans, "discover-orphans", false, "deprecated; PVC-only orphan discovery is always enabled")
	flag.DurationVar(&opts.wait, "wait", 2*time.Minute, "maximum time to wait for each PVC/PV deletion")
	flag.Parse()
	return opts
}

func run(ctx context.Context, opts options) error {
	if opts.namespace == "" {
		return errors.New("--namespace is required")
	}
	if err := rejectDeprecatedScopeFlags(opts); err != nil {
		return err
	}

	client, err := buildClient(opts.kubeconfig)
	if err != nil {
		return err
	}

	targets, err := collectTargets(ctx, client, opts)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		fmt.Println("no path/value PVC/PV targets found")
		return nil
	}

	sort.Slice(targets, func(i, j int) bool {
		if targets[i].namespace != targets[j].namespace {
			return targets[i].namespace < targets[j].namespace
		}
		return targets[i].name < targets[j].name
	})

	if opts.confirm {
		fmt.Printf("deleting %d path/value PVC target(s)\n", len(targets))
	} else {
		fmt.Printf("dry-run: found %d path/value PVC target(s); pass --confirm to delete\n", len(targets))
	}

	for _, target := range targets {
		printTarget(target)
		if !opts.confirm {
			continue
		}
		if err := deleteTarget(ctx, client, opts, target); err != nil {
			return err
		}
	}

	return nil
}

func collectTargets(ctx context.Context, client kubernetes.Interface, opts options) ([]targetPVC, error) {
	pvcs, err := client.CoreV1().PersistentVolumeClaims(opts.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	pods, err := client.CoreV1().Pods(opts.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	targets := collectPVCOnlyTargets(pvcs.Items, collectUsedPVCs(pods.Items))
	for i := range targets {
		pv, matched, err := lookupBoundPV(ctx, client, targets[i].pvc)
		if err != nil {
			return nil, err
		}
		targets[i].pv = pv
		targets[i].pvClaimRefMatched = matched
	}

	return targets, nil
}

func collectPVCOnlyTargets(pvcs []corev1.PersistentVolumeClaim, usedPVCs map[string]struct{}) []targetPVC {
	var targets []targetPVC
	for i := range pvcs {
		pvc := &pvcs[i]
		path, value, ok := templateStoreAnnotations(pvc)
		if !ok {
			continue
		}
		if hasOwnershipLabel(pvc) {
			continue
		}
		if _, ok := usedPVCs[namespacedName(pvc.Namespace, pvc.Name)]; ok {
			continue
		}
		targets = append(targets, targetPVC{
			namespace: pvc.Namespace,
			name:      pvc.Name,
			path:      path,
			value:     value,
			reason:    "PVC has path/value annotations, lacks template/app ownership labels, and is not referenced by any active pod",
			pvc:       pvc.DeepCopy(),
		})
	}

	return targets
}

func templateStoreAnnotations(pvc *corev1.PersistentVolumeClaim) (string, string, bool) {
	path := strings.TrimSpace(pvc.Annotations[pathAnnotationKey])
	value := strings.TrimSpace(pvc.Annotations[valueAnnotationKey])
	return path, value, path != "" && value != ""
}

func hasOwnershipLabel(pvc *corev1.PersistentVolumeClaim) bool {
	for _, key := range []string{templateDeployKey, appDeployKey, legacyAppLabelKey} {
		if _, ok := pvc.Labels[key]; ok {
			return true
		}
	}
	return false
}

func collectUsedPVCs(pods []corev1.Pod) map[string]struct{} {
	used := map[string]struct{}{}
	for i := range pods {
		pod := &pods[i]
		if isTerminalPod(pod) {
			continue
		}
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim == nil || volume.PersistentVolumeClaim.ClaimName == "" {
				continue
			}
			used[namespacedName(pod.Namespace, volume.PersistentVolumeClaim.ClaimName)] = struct{}{}
		}
	}
	return used
}

func isTerminalPod(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

func namespacedName(namespace, name string) string {
	return namespace + "/" + name
}

func rejectDeprecatedScopeFlags(opts options) error {
	var deprecated []string
	if opts.instance != "" {
		deprecated = append(deprecated, "--instance")
	}
	if len(opts.statefulSets) > 0 {
		deprecated = append(deprecated, "--statefulset")
	}
	if len(opts.claimTemplates) > 0 {
		deprecated = append(deprecated, "--claim-template")
	}
	if opts.discoverOrphans {
		deprecated = append(deprecated, "--discover-orphans")
	}
	if opts.allowNameOnly {
		deprecated = append(deprecated, "--allow-name-only")
	}
	if len(deprecated) == 0 {
		return nil
	}
	return fmt.Errorf("%s no longer scope cleanup; rerun without them to use PVC-only cleanup", strings.Join(deprecated, ", "))
}

func lookupBoundPV(ctx context.Context, client kubernetes.Interface, pvc *corev1.PersistentVolumeClaim) (*corev1.PersistentVolume, bool, error) {
	if pvc.Spec.VolumeName == "" {
		return nil, false, nil
	}

	pv, err := client.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	return pv, pvClaimRefMatches(pv, pvc), nil
}

func pvClaimRefMatches(pv *corev1.PersistentVolume, pvc *corev1.PersistentVolumeClaim) bool {
	ref := pv.Spec.ClaimRef
	if ref == nil {
		return false
	}
	if ref.Namespace != pvc.Namespace || ref.Name != pvc.Name {
		return false
	}
	if ref.UID != "" && pvc.UID != "" && ref.UID != pvc.UID {
		return false
	}
	return true
}

func deleteTarget(ctx context.Context, client kubernetes.Interface, opts options, target targetPVC) error {
	if target.pv != nil && !target.pvClaimRefMatched {
		return fmt.Errorf("refusing to delete PVC %s/%s because PV %s claimRef does not match", target.namespace, target.name, target.pv.Name)
	}

	err := client.CoreV1().PersistentVolumeClaims(target.namespace).Delete(ctx, target.name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err := waitForPVCGone(ctx, client, target.namespace, target.name, opts.wait); err != nil {
		return err
	}

	if opts.deletePV && target.pv != nil {
		return deletePVIfStillLeft(ctx, client, target, opts.wait)
	}

	return nil
}

func deletePVIfStillLeft(ctx context.Context, client kubernetes.Interface, target targetPVC, timeout time.Duration) error {
	pv, err := client.CoreV1().PersistentVolumes().Get(ctx, target.pv.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !pvClaimRefStillMatchesCapturedPVC(pv, target.pvc) {
		return fmt.Errorf("refusing to delete PV %s because its claimRef changed", pv.Name)
	}

	err = client.CoreV1().PersistentVolumes().Delete(ctx, pv.Name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return waitForPVGone(ctx, client, pv.Name, timeout)
}

func pvClaimRefStillMatchesCapturedPVC(pv *corev1.PersistentVolume, pvc *corev1.PersistentVolumeClaim) bool {
	ref := pv.Spec.ClaimRef
	if ref == nil {
		return false
	}
	return ref.Namespace == pvc.Namespace && ref.Name == pvc.Name && ref.UID == pvc.UID
}

func waitForPVCGone(ctx context.Context, client kubernetes.Interface, namespace, name string, timeout time.Duration) error {
	return waitForGone(ctx, timeout, func(ctx context.Context) (bool, error) {
		_, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}, fmt.Sprintf("PVC %s/%s", namespace, name))
}

func waitForPVGone(ctx context.Context, client kubernetes.Interface, name string, timeout time.Duration) error {
	return waitForGone(ctx, timeout, func(ctx context.Context) (bool, error) {
		_, err := client.CoreV1().PersistentVolumes().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}, fmt.Sprintf("PV %s", name))
}

func waitForGone(ctx context.Context, timeout time.Duration, check func(context.Context) (bool, error), subject string) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()

	for {
		gone, err := check(ctx)
		if err != nil {
			return err
		}
		if gone {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for %s to be deleted", subject)
		case <-tick.C:
		}
	}
}

func printTarget(target targetPVC) {
	fmt.Printf("- pvc=%s/%s path=%q value=%q reason=%q\n",
		target.namespace,
		target.name,
		target.path,
		target.value,
		target.reason,
	)
	if target.pv == nil {
		fmt.Println("  pv=<none>")
		return
	}
	fmt.Printf("  pv=%s phase=%s reclaimPolicy=%s claimRefMatched=%t\n",
		target.pv.Name,
		target.pv.Status.Phase,
		target.pv.Spec.PersistentVolumeReclaimPolicy,
		target.pvClaimRefMatched,
	)
}

func buildClient(kubeconfig string) (kubernetes.Interface, error) {
	config, err := buildRESTConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(config)
}

func buildRESTConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	config, err := rest.InClusterConfig()
	if err == nil {
		return config, nil
	}
	return clientcmd.BuildConfigFromFlags("", defaultKubeconfig())
}

func defaultKubeconfig() string {
	if value := os.Getenv("KUBECONFIG"); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".kube", "config")
}
