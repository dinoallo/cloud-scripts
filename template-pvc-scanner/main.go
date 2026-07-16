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
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
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

	outputTable = "table"
	outputCSV   = "csv"
	outputJSONL = "jsonl"
)

var outputHeaders = []string{
	"namespace",
	"pvc",
	"pv",
	"statefulset",
	"claimTemplate",
	"reason",
	"pvcPhase",
	"pvPhase",
	"pvReclaimPolicy",
	"pvClaimRefMatched",
	"pvcStorageClass",
	"pvcSize",
	"pvcAge",
}

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
	output          string
	statefulSets    stringList
	claimTemplates  stringList
	discoverOrphans bool
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

type outputRow struct {
	Namespace         string `json:"namespace"`
	PVC               string `json:"pvc"`
	PV                string `json:"pv"`
	StatefulSet       string `json:"statefulset"`
	ClaimTemplate     string `json:"claimTemplate"`
	Reason            string `json:"reason"`
	PVCPhase          string `json:"pvcPhase"`
	PVPhase           string `json:"pvPhase"`
	PVReclaimPolicy   string `json:"pvReclaimPolicy"`
	PVClaimRefMatched bool   `json:"pvClaimRefMatched"`
	PVCStorageClass   string `json:"pvcStorageClass"`
	PVCSize           string `json:"pvcSize"`
	PVCAge            string `json:"pvcAge"`
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
	flag.StringVar(&opts.namespace, "namespace", "", "namespace to scan; empty scans all namespaces")
	flag.StringVar(&opts.instance, "instance", "", "template instance name used by cloud.sealos.io/deploy-on-sealos")
	flag.StringVar(&opts.output, "output", outputTable, "output format: table, csv, or jsonl")
	flag.Var(&opts.statefulSets, "statefulset", "StatefulSet name to inspect; repeat for multiple values")
	flag.Var(&opts.claimTemplates, "claim-template", "volumeClaimTemplate name for orphan scanning; repeat for multiple values")
	flag.BoolVar(&opts.discoverOrphans, "discover-orphans", false, "discover orphan StatefulSet PVCs from legacy app labels when the StatefulSet name is unknown")
	flag.Parse()
	return opts
}

func run(ctx context.Context, opts options) error {
	if opts.instance == "" {
		return errors.New("--instance is required to keep scanning scoped to one template instance")
	}
	if err := validateOutputFormat(opts.output); err != nil {
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
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].namespace != targets[j].namespace {
			return targets[i].namespace < targets[j].namespace
		}
		return targets[i].name < targets[j].name
	})

	return writeOutput(os.Stdout, targets, opts.output, time.Now())
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
		liveNames[namespacedName(sts.Namespace, sts.Name)] = struct{}{}
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
		if _, exists := liveNames[namespacedName(pvc.Namespace, statefulSetName)]; exists {
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
			namespace:     pvc.Namespace,
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
				if pvcs[i].Namespace != sts.Namespace {
					continue
				}
				if !isStatefulSetPVC(pvcs[i].Name, sts.Name, claimTemplate.Name) {
					continue
				}
				if !isBugAffectedPVC(&pvcs[i]) {
					continue
				}
				targets = append(targets, targetPVC{
					namespace:     pvcs[i].Namespace,
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
		return nil, errors.New("--statefulset and --claim-template must be provided together for orphan scanning")
	}

	liveNames := map[string]struct{}{}
	live, err := client.AppsV1().StatefulSets(opts.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, sts := range live.Items {
		liveNames[namespacedName(sts.Namespace, sts.Name)] = struct{}{}
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

				_, statefulSetStillExists := liveNames[namespacedName(pvcs[i].Namespace, stsName)]
				if !statefulSetStillExists && !hasLegacyAppEvidence(&pvcs[i], stsName, opts.instance) {
					continue
				}

				targets = append(targets, targetPVC{
					namespace:     pvcs[i].Namespace,
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

func namespacedName(namespace, name string) string {
	return namespace + "/" + name
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

func validateOutputFormat(format string) error {
	switch format {
	case outputTable, outputCSV, outputJSONL:
		return nil
	default:
		return fmt.Errorf("--output must be one of %s, %s, or %s", outputTable, outputCSV, outputJSONL)
	}
}

func writeOutput(w io.Writer, targets []targetPVC, format string, now time.Time) error {
	rows := makeOutputRows(targets, now)

	switch format {
	case outputTable:
		return writeTableOutput(w, rows)
	case outputCSV:
		return writeCSVOutput(w, rows)
	case outputJSONL:
		return writeJSONLOutput(w, rows)
	default:
		return validateOutputFormat(format)
	}
}

func makeOutputRows(targets []targetPVC, now time.Time) []outputRow {
	rows := make([]outputRow, 0, len(targets))
	for _, target := range targets {
		rows = append(rows, makeOutputRow(target, now))
	}
	return rows
}

func makeOutputRow(target targetPVC, now time.Time) outputRow {
	row := outputRow{
		Namespace:         target.namespace,
		PVC:               target.name,
		StatefulSet:       target.statefulSet,
		ClaimTemplate:     target.claimTemplate,
		Reason:            target.reason,
		PVClaimRefMatched: target.pvClaimRefMatched,
	}

	if target.pvc != nil {
		row.Namespace = target.pvc.Namespace
		row.PVC = target.pvc.Name
		row.PVCPhase = string(target.pvc.Status.Phase)
		row.PVCStorageClass = storageClassName(target.pvc)
		row.PVCSize = storageRequest(target.pvc)
		row.PVCAge = formatAge(target.pvc.CreationTimestamp, now)
	}
	if target.pv != nil {
		row.PV = target.pv.Name
		row.PVPhase = string(target.pv.Status.Phase)
		row.PVReclaimPolicy = string(target.pv.Spec.PersistentVolumeReclaimPolicy)
	}

	return row
}

func writeTableOutput(w io.Writer, rows []outputRow) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "no bug-affected PVC/PV targets found")
		return err
	}

	if _, err := fmt.Fprintf(w, "found %d bug-affected PVC/PV candidate(s)\n", len(rows)); err != nil {
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if err := writeTabRecord(tw, outputHeaders); err != nil {
		return err
	}
	for _, row := range rows {
		if err := writeTabRecord(tw, row.csvRecord()); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func writeTabRecord(w io.Writer, record []string) error {
	_, err := fmt.Fprintln(w, strings.Join(record, "\t"))
	return err
}

func writeCSVOutput(w io.Writer, rows []outputRow) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(outputHeaders); err != nil {
		return err
	}
	for _, row := range rows {
		if err := cw.Write(row.csvRecord()); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func writeJSONLOutput(w io.Writer, rows []outputRow) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	for _, row := range rows {
		if err := encoder.Encode(row); err != nil {
			return err
		}
	}
	return nil
}

func (r outputRow) csvRecord() []string {
	return []string{
		r.Namespace,
		r.PVC,
		r.PV,
		r.StatefulSet,
		r.ClaimTemplate,
		r.Reason,
		r.PVCPhase,
		r.PVPhase,
		r.PVReclaimPolicy,
		fmt.Sprintf("%t", r.PVClaimRefMatched),
		r.PVCStorageClass,
		r.PVCSize,
		r.PVCAge,
	}
}

func storageClassName(pvc *corev1.PersistentVolumeClaim) string {
	if pvc.Spec.StorageClassName == nil {
		return ""
	}
	return *pvc.Spec.StorageClassName
}

func storageRequest(pvc *corev1.PersistentVolumeClaim) string {
	if pvc.Spec.Resources.Requests == nil {
		return ""
	}
	quantity, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if !ok || quantity.IsZero() {
		return ""
	}
	return quantity.String()
}

func formatAge(created metav1.Time, now time.Time) string {
	if created.IsZero() {
		return ""
	}

	age := now.Sub(created.Time)
	if age < 0 {
		age = 0
	}

	days := int(age.Hours()) / 24
	hours := int(age.Hours()) % 24
	minutes := int(age.Minutes()) % 60
	seconds := int(age.Seconds()) % 60

	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%dm%ds", minutes, seconds)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
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
