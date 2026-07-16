package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	templateDeployKey = "cloud.sealos.io/deploy-on-sealos"
	legacyAppLabelKey = "app"
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
	statefulSet       string
	claimTemplate     string
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
	flag.StringVar(&opts.instance, "instance", "", "template instance name used by cloud.sealos.io/deploy-on-sealos")
	flag.Var(&opts.statefulSets, "statefulset", "StatefulSet name to inspect; repeat for multiple values")
	flag.Var(&opts.claimTemplates, "claim-template", "volumeClaimTemplate name for orphan cleanup; repeat for multiple values")
	flag.BoolVar(&opts.confirm, "confirm", false, "delete matching PVCs/PVs; without this flag the program only prints a dry-run plan")
	flag.BoolVar(&opts.deletePV, "delete-pv", true, "delete a leftover PV after its target PVC is gone and the PV claimRef still matches")
	flag.BoolVar(&opts.allowNameOnly, "allow-name-only", false, "allow orphan cleanup by PVC name pattern without a legacy app label match")
	flag.BoolVar(&opts.discoverOrphans, "discover-orphans", false, "discover orphan StatefulSet PVCs from legacy app labels when the StatefulSet name is unknown")
	flag.DurationVar(&opts.wait, "wait", 2*time.Minute, "maximum time to wait for each PVC/PV deletion")
	flag.Parse()
	return opts
}

func run(ctx context.Context, opts options) error {
	if opts.namespace == "" {
		return errors.New("--namespace is required")
	}
	if opts.instance == "" {
		return errors.New("--instance is required to keep cleanup scoped to one template instance")
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
		fmt.Println("no bug-affected PVC/PV targets found")
		return nil
	}

	sort.Slice(targets, func(i, j int) bool {
		return targets[i].name < targets[j].name
	})

	if opts.confirm {
		fmt.Printf("deleting %d bug-affected PVC target(s)\n", len(targets))
	} else {
		fmt.Printf("dry-run: found %d bug-affected PVC target(s); pass --confirm to delete\n", len(targets))
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

	var targets []targetPVC
	seen := map[string]struct{}{}

	add := func(target targetPVC) {
		key := target.namespace + "/" + target.name
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		targets = append(targets, target)
	}

	liveTargets, err := collectLiveStatefulSetTargets(ctx, client, opts, pvcs.Items)
	if err != nil {
		return nil, err
	}
	for _, target := range liveTargets {
		add(target)
	}

	orphanTargets, err := collectExplicitOrphanTargets(ctx, client, opts, pvcs.Items)
	if err != nil {
		return nil, err
	}
	for _, target := range orphanTargets {
		add(target)
	}

	discoveredTargets, err := collectDiscoveredOrphanTargets(ctx, client, opts, pvcs.Items)
	if err != nil {
		return nil, err
	}
	for _, target := range discoveredTargets {
		add(target)
	}

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

func collectDiscoveredOrphanTargets(ctx context.Context, client kubernetes.Interface, opts options, pvcs []corev1.PersistentVolumeClaim) ([]targetPVC, error) {
	if !opts.discoverOrphans {
		return nil, nil
	}

	liveNames := map[string]struct{}{}
	live, err := client.AppsV1().StatefulSets(opts.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, sts := range live.Items {
		liveNames[sts.Name] = struct{}{}
	}

	requestedStatefulSets := toSet(opts.statefulSets)
	requestedClaimTemplates := toSet(opts.claimTemplates)

	var targets []targetPVC
	for i := range pvcs {
		pvc := &pvcs[i]
		if !isBugAffectedPVC(pvc) {
			continue
		}

		statefulSetName := pvc.Labels[legacyAppLabelKey]
		if statefulSetName == "" {
			continue
		}
		if _, exists := liveNames[statefulSetName]; exists {
			continue
		}
		if len(requestedStatefulSets) > 0 {
			if _, ok := requestedStatefulSets[statefulSetName]; !ok {
				continue
			}
		}

		claimTemplate, ok := inferClaimTemplateFromLegacyAppPVC(pvc.Name, statefulSetName)
		if !ok {
			continue
		}
		if len(requestedClaimTemplates) > 0 {
			if _, ok := requestedClaimTemplates[claimTemplate]; !ok {
				continue
			}
		}

		targets = append(targets, targetPVC{
			namespace:     opts.namespace,
			name:          pvc.Name,
			statefulSet:   statefulSetName,
			claimTemplate: claimTemplate,
			reason:        "discovered orphan StatefulSet PVC from legacy app label and StatefulSet PVC name",
			pvc:           pvc.DeepCopy(),
		})
	}

	return targets, nil
}

func collectLiveStatefulSetTargets(ctx context.Context, client kubernetes.Interface, opts options, pvcs []corev1.PersistentVolumeClaim) ([]targetPVC, error) {
	stsList, err := client.AppsV1().StatefulSets(opts.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", templateDeployKey, opts.instance),
	})
	if err != nil {
		return nil, err
	}

	requested := toSet(opts.statefulSets)
	var targets []targetPVC

	for _, sts := range stsList.Items {
		if len(requested) > 0 {
			if _, ok := requested[sts.Name]; !ok {
				continue
			}
		}
		if sts.Spec.PersistentVolumeClaimRetentionPolicy != nil &&
			sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted == "Delete" {
			continue
		}

		for _, claimTemplate := range sts.Spec.VolumeClaimTemplates {
			if claimTemplate.Labels[templateDeployKey] == opts.instance {
				continue
			}

			for i := range pvcs {
				if !isStatefulSetPVC(pvcs[i].Name, sts.Name, claimTemplate.Name) {
					continue
				}
				if !isBugAffectedPVC(&pvcs[i]) {
					continue
				}
				targets = append(targets, targetPVC{
					namespace:     opts.namespace,
					name:          pvcs[i].Name,
					statefulSet:   sts.Name,
					claimTemplate: claimTemplate.Name,
					reason:        "live StatefulSet has instance label but its volumeClaimTemplate lacks that label",
					pvc:           pvcs[i].DeepCopy(),
				})
			}
		}
	}

	return targets, nil
}

func collectExplicitOrphanTargets(ctx context.Context, client kubernetes.Interface, opts options, pvcs []corev1.PersistentVolumeClaim) ([]targetPVC, error) {
	if len(opts.statefulSets) == 0 && len(opts.claimTemplates) == 0 {
		return nil, nil
	}
	if len(opts.statefulSets) == 0 || len(opts.claimTemplates) == 0 {
		return nil, errors.New("--statefulset and --claim-template must be provided together for orphan cleanup")
	}

	liveNames := map[string]struct{}{}
	live, err := client.AppsV1().StatefulSets(opts.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, sts := range live.Items {
		liveNames[sts.Name] = struct{}{}
	}

	var targets []targetPVC
	for _, stsName := range opts.statefulSets {
		for _, claimTemplate := range opts.claimTemplates {
			for i := range pvcs {
				if !isStatefulSetPVC(pvcs[i].Name, stsName, claimTemplate) {
					continue
				}
				if !isBugAffectedPVC(&pvcs[i]) {
					continue
				}

				_, statefulSetStillExists := liveNames[stsName]
				if !statefulSetStillExists && !opts.allowNameOnly && !hasLegacyAppEvidence(&pvcs[i], stsName, opts.instance) {
					continue
				}

				targets = append(targets, targetPVC{
					namespace:     opts.namespace,
					name:          pvcs[i].Name,
					statefulSet:   stsName,
					claimTemplate: claimTemplate,
					reason:        "explicit orphan StatefulSet PVC pattern with legacy app label evidence",
					pvc:           pvcs[i].DeepCopy(),
				})
			}
		}
	}

	return targets, nil
}

func isBugAffectedPVC(pvc *corev1.PersistentVolumeClaim) bool {
	_, hasTemplateDeployLabel := pvc.Labels[templateDeployKey]
	return !hasTemplateDeployLabel
}

func hasLegacyAppEvidence(pvc *corev1.PersistentVolumeClaim, statefulSetName, instance string) bool {
	value := pvc.Labels[legacyAppLabelKey]
	return value == statefulSetName || value == instance
}

func isStatefulSetPVC(pvcName, statefulSetName, claimTemplate string) bool {
	pattern := fmt.Sprintf("^%s-%s-[0-9]+$", regexp.QuoteMeta(claimTemplate), regexp.QuoteMeta(statefulSetName))
	return regexp.MustCompile(pattern).MatchString(pvcName)
}

func inferClaimTemplateFromLegacyAppPVC(pvcName, legacyApp string) (string, bool) {
	pattern := fmt.Sprintf("^(.+)-%s-[0-9]+$", regexp.QuoteMeta(legacyApp))
	matches := regexp.MustCompile(pattern).FindStringSubmatch(pvcName)
	if len(matches) != 2 || matches[1] == "" {
		return "", false
	}
	return matches[1], true
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
	fmt.Printf("- pvc=%s/%s statefulset=%s claimTemplate=%s reason=%q\n",
		target.namespace,
		target.name,
		target.statefulSet,
		target.claimTemplate,
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

func toSet(items []string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, item := range items {
		out[item] = struct{}{}
	}
	return out
}
