package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	templateDeployKey  = "cloud.sealos.io/deploy-on-sealos"
	appDeployKey       = "cloud.sealos.io/app-deploy-manager"
	pathAnnotationKey  = "path"
	valueAnnotationKey = "value"

	outputTable = "table"
	outputCSV   = "csv"
	outputJSONL = "jsonl"
)

var outputHeaders = []string{
	"namespace",
	"pvc",
	"path",
	"value",
	"reason",
	"pvcPhase",
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
	output          string
	instance        string
	statefulSets    stringList
	claimTemplates  stringList
	discoverOrphans bool
}

type targetPVC struct {
	namespace string
	name      string
	path      string
	value     string
	reason    string
	pvc       *corev1.PersistentVolumeClaim
}

type outputRow struct {
	Namespace       string `json:"namespace"`
	PVC             string `json:"pvc"`
	Path            string `json:"path"`
	Value           string `json:"value"`
	Reason          string `json:"reason"`
	PVCPhase        string `json:"pvcPhase"`
	PVCStorageClass string `json:"pvcStorageClass"`
	PVCSize         string `json:"pvcSize"`
	PVCAge          string `json:"pvcAge"`
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
	flag.StringVar(&opts.output, "output", outputTable, "output format: table, csv, or jsonl")
	flag.StringVar(&opts.instance, "instance", "", "deprecated and ignored; PVC-only scanning does not use template instance scope")
	flag.Var(&opts.statefulSets, "statefulset", "deprecated and ignored; PVC-only scanning does not inspect StatefulSets")
	flag.Var(&opts.claimTemplates, "claim-template", "deprecated and ignored; PVC-only scanning does not inspect volumeClaimTemplates")
	flag.BoolVar(&opts.discoverOrphans, "discover-orphans", false, "deprecated and ignored; PVC-only orphan discovery is always enabled")
	flag.Parse()
	return opts
}

func run(ctx context.Context, opts options) error {
	if err := validateOutputFormat(opts.output); err != nil {
		return err
	}
	warnDeprecatedScopeFlags(opts)

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
	pods, err := client.CoreV1().Pods(opts.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	return collectPVCOnlyTargets(pvcs.Items, collectUsedPVCs(pods.Items)), nil
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
			reason:    "PVC has path/value annotations, lacks Sealos template ownership labels, and is not referenced by any active pod",
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
	for _, key := range []string{templateDeployKey, appDeployKey} {
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

func warnDeprecatedScopeFlags(opts options) {
	if opts.instance == "" && len(opts.statefulSets) == 0 && len(opts.claimTemplates) == 0 && !opts.discoverOrphans {
		return
	}
	fmt.Fprintln(os.Stderr, "warning: --instance, --statefulset, --claim-template, and --discover-orphans are deprecated and ignored; template-pvc-scanner now uses PVC-only discovery")
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
		Namespace: target.namespace,
		PVC:       target.name,
		Path:      target.path,
		Value:     target.value,
		Reason:    target.reason,
	}

	if target.pvc != nil {
		row.Namespace = target.pvc.Namespace
		row.PVC = target.pvc.Name
		row.PVCPhase = string(target.pvc.Status.Phase)
		row.PVCStorageClass = storageClassName(target.pvc)
		row.PVCSize = storageRequest(target.pvc)
		row.PVCAge = formatAge(target.pvc.CreationTimestamp, now)
	}
	return row
}

func writeTableOutput(w io.Writer, rows []outputRow) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "no path/value PVC candidates found")
		return err
	}

	if _, err := fmt.Fprintf(w, "found %d path/value PVC candidate(s)\n", len(rows)); err != nil {
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
		r.Path,
		r.Value,
		r.Reason,
		r.PVCPhase,
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
