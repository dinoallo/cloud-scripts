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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	outputTable = "table"
	outputCSV   = "csv"
	outputJSONL = "jsonl"
)

const (
	ownerFound             = "ownerFound"
	ownerNoReferences      = "noOwnerReferences"
	ownerNotFound          = "ownerNotFound"
	ownerUIDMismatch       = "ownerUIDMismatch"
	ownerGVKNotFound       = "ownerGVKNotFound"
	ownerInvalidScope      = "ownerInvalidScope"
	ownerLookupError       = "ownerLookupError"
	ownerLookupErrorReason = "owner lookup failed; PVC orphan status is not authoritative"
)

var outputHeaders = []string{
	"namespace",
	"pvc",
	"pv",
	"ownerStatus",
	"reason",
	"pvcPhase",
	"pvPhase",
	"pvReclaimPolicy",
	"pvClaimRefMatched",
	"pvcStorageClass",
}

type options struct {
	kubeconfig    string
	namespace     string
	output        string
	resolveOwners bool
}

type clients struct {
	kubernetes kubernetes.Interface
	dynamic    dynamic.Interface
	mapper     meta.RESTMapper
}

type targetPVC struct {
	namespace         string
	name              string
	owner             ownerCheck
	ownerRefCount     int
	pvc               *corev1.PersistentVolumeClaim
	pv                *corev1.PersistentVolume
	pvClaimRefMatched bool
}

type ownerCheck struct {
	status                  string
	apiVersion              string
	kind                    string
	namespace               string
	name                    string
	uid                     string
	controller              bool
	blockOwnerDeletion      bool
	reason                  string
	crossNamespaceCheckUsed bool
}

type outputRow struct {
	Namespace               string `json:"namespace"`
	PVC                     string `json:"pvc"`
	PV                      string `json:"pv"`
	OwnerStatus             string `json:"ownerStatus"`
	OwnerAPIVersion         string `json:"ownerAPIVersion"`
	OwnerKind               string `json:"ownerKind"`
	OwnerNamespace          string `json:"ownerNamespace"`
	OwnerName               string `json:"ownerName"`
	OwnerUID                string `json:"ownerUID"`
	OwnerController         bool   `json:"ownerController"`
	OwnerBlockOwnerDeletion bool   `json:"ownerBlockOwnerDeletion"`
	OwnerRefCount           int    `json:"ownerRefCount"`
	Reason                  string `json:"reason"`
	PVCPhase                string `json:"pvcPhase"`
	PVPhase                 string `json:"pvPhase"`
	PVReclaimPolicy         string `json:"pvReclaimPolicy"`
	PVClaimRefMatched       bool   `json:"pvClaimRefMatched"`
	PVCStorageClass         string `json:"pvcStorageClass"`
	PVCSize                 string `json:"pvcSize"`
	PVCAge                  string `json:"pvcAge"`
}

type ownerResolver interface {
	ResolveOwner(ctx context.Context, pvc *corev1.PersistentVolumeClaim, ref metav1.OwnerReference) ownerCheck
}

type dynamicOwnerResolver struct {
	client      dynamic.Interface
	mapper      meta.RESTMapper
	lookupCache map[ownerLookupKey]ownerLookupResult
	scopeCache  map[crossNamespaceKey]crossNamespaceResult
}

type ownerLookupKey struct {
	resource  schema.GroupVersionResource
	namespace string
	name      string
}

type ownerLookupResult struct {
	object *unstructured.Unstructured
	err    error
}

type crossNamespaceKey struct {
	resource     schema.GroupVersionResource
	pvcNamespace string
	name         string
	uid          types.UID
}

type crossNamespaceResult struct {
	namespace string
	found     bool
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
	flag.BoolVar(&opts.resolveOwners, "resolve-owners", false, "resolve ownerReferences by querying owner objects; requires additional RBAC")
	flag.Parse()
	return opts
}

func run(ctx context.Context, opts options) error {
	if err := validateOutputFormat(opts.output); err != nil {
		return err
	}

	clients, err := buildClients(opts.kubeconfig)
	if err != nil {
		return err
	}

	var resolver ownerResolver
	if opts.resolveOwners {
		resolver = &dynamicOwnerResolver{
			client:      clients.dynamic,
			mapper:      clients.mapper,
			lookupCache: map[ownerLookupKey]ownerLookupResult{},
			scopeCache:  map[crossNamespaceKey]crossNamespaceResult{},
		}
	}

	targets, err := collectTargets(ctx, clients.kubernetes, resolver, opts)
	if err != nil {
		return err
	}
	sortTargets(targets)

	return writeOutput(os.Stdout, targets, opts.output, time.Now())
}

func collectTargets(ctx context.Context, client kubernetes.Interface, resolver ownerResolver, opts options) ([]targetPVC, error) {
	pvcs, err := client.CoreV1().PersistentVolumeClaims(opts.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	usedPVCs, err := collectUsedPVCs(ctx, client, opts.namespace)
	if err != nil {
		return nil, fmt.Errorf("list pods to identify in-use PVCs: %w", err)
	}

	targets := make([]targetPVC, 0)
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		owner, orphan := classifyPVC(ctx, pvc, resolver, opts.resolveOwners)
		if !orphan {
			continue
		}
		if owner.status == ownerNoReferences && usedPVCs.Has(pvc.Namespace, pvc.Name) {
			continue
		}

		targets = append(targets, targetPVC{
			namespace:     pvc.Namespace,
			name:          pvc.Name,
			owner:         owner,
			ownerRefCount: len(pvc.OwnerReferences),
			pvc:           pvc.DeepCopy(),
		})
	}

	if !hasBoundPVC(targets) {
		return targets, nil
	}

	pvs, err := collectPVs(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("list PVs to identify bound volumes: %w", err)
	}

	for i := range targets {
		pv, matched := pvs.BoundPV(targets[i].pvc)
		targets[i].pv = pv
		targets[i].pvClaimRefMatched = matched
	}

	return targets, nil
}

func hasBoundPVC(targets []targetPVC) bool {
	for _, target := range targets {
		if target.pvc != nil && target.pvc.Spec.VolumeName != "" {
			return true
		}
	}
	return false
}

type pvcUsageIndex map[types.NamespacedName]struct{}

func collectUsedPVCs(ctx context.Context, client kubernetes.Interface, namespace string) (pvcUsageIndex, error) {
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	used := pvcUsageIndex{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if isTerminalPod(pod) {
			continue
		}
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim == nil || volume.PersistentVolumeClaim.ClaimName == "" {
				continue
			}
			used[types.NamespacedName{
				Namespace: pod.Namespace,
				Name:      volume.PersistentVolumeClaim.ClaimName,
			}] = struct{}{}
		}
	}
	return used, nil
}

func (i pvcUsageIndex) Has(namespace, name string) bool {
	_, ok := i[types.NamespacedName{Namespace: namespace, Name: name}]
	return ok
}

func isTerminalPod(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

func classifyPVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim, resolver ownerResolver, resolveOwners bool) (ownerCheck, bool) {
	if len(pvc.OwnerReferences) == 0 {
		return ownerCheck{
			status: ownerNoReferences,
			reason: "PVC has no ownerReferences",
		}, true
	}
	if !resolveOwners {
		return ownerCheck{}, false
	}
	if resolver == nil {
		return ownerCheck{
			status: ownerLookupError,
			reason: "owner lookup requested but resolver is not configured",
		}, true
	}

	checks := make([]ownerCheck, 0, len(pvc.OwnerReferences))
	for _, ref := range pvc.OwnerReferences {
		check := resolver.ResolveOwner(ctx, pvc, ref)
		if check.status == ownerFound {
			return ownerCheck{}, false
		}
		checks = append(checks, check)
	}

	return selectOwnerCheck(checks), true
}

func selectOwnerCheck(checks []ownerCheck) ownerCheck {
	priorities := []string{
		ownerLookupError,
		ownerInvalidScope,
		ownerGVKNotFound,
		ownerNotFound,
		ownerUIDMismatch,
	}

	for _, status := range priorities {
		for _, check := range checks {
			if check.status == status {
				return check
			}
		}
	}
	if len(checks) > 0 {
		return checks[0]
	}
	return ownerCheck{
		status: ownerLookupError,
		reason: ownerLookupErrorReason,
	}
}

func (r *dynamicOwnerResolver) ResolveOwner(ctx context.Context, pvc *corev1.PersistentVolumeClaim, ref metav1.OwnerReference) ownerCheck {
	check := ownerCheckFromReference(pvc, ref)

	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		check.status = ownerGVKNotFound
		check.reason = fmt.Sprintf("owner apiVersion %q is invalid: %v", ref.APIVersion, err)
		return check
	}

	mapping, err := r.mapper.RESTMapping(gv.WithKind(ref.Kind).GroupKind(), gv.Version)
	if meta.IsNoMatchError(err) {
		check.status = ownerGVKNotFound
		check.reason = fmt.Sprintf("owner GVK %s/%s is not available from discovery", ref.APIVersion, ref.Kind)
		return check
	}
	if err != nil {
		check.status = ownerLookupError
		check.reason = fmt.Sprintf("%s: %v", ownerLookupErrorReason, err)
		return check
	}

	switch mapping.Scope.Name() {
	case meta.RESTScopeNameNamespace:
		check.namespace = pvc.Namespace
		return r.resolveNamespacedOwner(ctx, pvc, ref, mapping.Resource, check)
	case meta.RESTScopeNameRoot:
		check.namespace = ""
		return r.resolveClusterOwner(ctx, ref, mapping.Resource, check)
	default:
		check.status = ownerInvalidScope
		check.reason = fmt.Sprintf("owner GVK %s/%s has unsupported REST scope %q", ref.APIVersion, ref.Kind, mapping.Scope.Name())
		return check
	}
}

func (r *dynamicOwnerResolver) resolveNamespacedOwner(ctx context.Context, pvc *corev1.PersistentVolumeClaim, ref metav1.OwnerReference, resource schema.GroupVersionResource, check ownerCheck) ownerCheck {
	object, err := r.getOwner(ctx, resource, pvc.Namespace, ref.Name)
	if apierrors.IsNotFound(err) {
		if namespace, found := r.findOwnerInOtherNamespace(ctx, resource, pvc.Namespace, ref); found {
			check.status = ownerInvalidScope
			check.namespace = namespace
			check.crossNamespaceCheckUsed = true
			check.reason = fmt.Sprintf("owner UID exists in namespace %q, but PVC ownerReferences can only point to same-namespace namespaced owners", namespace)
			return check
		}
		check.status = ownerNotFound
		check.reason = "owner object was not found in the PVC namespace"
		return check
	}
	if err != nil {
		check.status = ownerLookupError
		check.reason = fmt.Sprintf("%s: %v", ownerLookupErrorReason, err)
		return check
	}
	if object.GetUID() != ref.UID {
		if namespace, found := r.findOwnerInOtherNamespace(ctx, resource, pvc.Namespace, ref); found {
			check.status = ownerInvalidScope
			check.namespace = namespace
			check.crossNamespaceCheckUsed = true
			check.reason = fmt.Sprintf("owner UID exists in namespace %q, but PVC ownerReferences can only point to same-namespace namespaced owners", namespace)
			return check
		}
		check.status = ownerUIDMismatch
		check.reason = fmt.Sprintf("owner name exists in namespace %q, but UID is %q", pvc.Namespace, object.GetUID())
		return check
	}

	check.status = ownerFound
	check.reason = "owner object exists and UID matches"
	return check
}

func (r *dynamicOwnerResolver) resolveClusterOwner(ctx context.Context, ref metav1.OwnerReference, resource schema.GroupVersionResource, check ownerCheck) ownerCheck {
	object, err := r.getOwner(ctx, resource, "", ref.Name)
	if apierrors.IsNotFound(err) {
		check.status = ownerNotFound
		check.reason = "cluster-scoped owner object was not found"
		return check
	}
	if err != nil {
		check.status = ownerLookupError
		check.reason = fmt.Sprintf("%s: %v", ownerLookupErrorReason, err)
		return check
	}
	if object.GetUID() != ref.UID {
		check.status = ownerUIDMismatch
		check.reason = fmt.Sprintf("cluster-scoped owner name exists, but UID is %q", object.GetUID())
		return check
	}

	check.status = ownerFound
	check.reason = "cluster-scoped owner object exists and UID matches"
	return check
}

func (r *dynamicOwnerResolver) getOwner(ctx context.Context, resource schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	key := ownerLookupKey{
		resource:  resource,
		namespace: namespace,
		name:      name,
	}
	if cached, ok := r.lookupCache[key]; ok {
		return cached.object, cached.err
	}

	var (
		object *unstructured.Unstructured
		err    error
	)
	if namespace == "" {
		object, err = r.client.Resource(resource).Get(ctx, name, metav1.GetOptions{})
	} else {
		object, err = r.client.Resource(resource).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	}
	if object != nil {
		object = object.DeepCopy()
	}

	r.lookupCache[key] = ownerLookupResult{object: object, err: err}
	return object, err
}

func (r *dynamicOwnerResolver) findOwnerInOtherNamespace(ctx context.Context, resource schema.GroupVersionResource, pvcNamespace string, ref metav1.OwnerReference) (string, bool) {
	if ref.UID == "" {
		return "", false
	}

	key := crossNamespaceKey{
		resource:     resource,
		pvcNamespace: pvcNamespace,
		name:         ref.Name,
		uid:          ref.UID,
	}
	if cached, ok := r.scopeCache[key]; ok {
		return cached.namespace, cached.found
	}

	list, err := r.client.Resource(resource).List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", ref.Name).String(),
	})
	if err != nil {
		r.scopeCache[key] = crossNamespaceResult{}
		return "", false
	}

	for _, item := range list.Items {
		if item.GetUID() == ref.UID && item.GetNamespace() != "" && item.GetNamespace() != pvcNamespace {
			result := crossNamespaceResult{namespace: item.GetNamespace(), found: true}
			r.scopeCache[key] = result
			return result.namespace, result.found
		}
	}

	r.scopeCache[key] = crossNamespaceResult{}
	return "", false
}

func ownerCheckFromReference(pvc *corev1.PersistentVolumeClaim, ref metav1.OwnerReference) ownerCheck {
	check := ownerCheck{
		apiVersion: ref.APIVersion,
		kind:       ref.Kind,
		namespace:  pvc.Namespace,
		name:       ref.Name,
		uid:        string(ref.UID),
	}
	if ref.Controller != nil {
		check.controller = *ref.Controller
	}
	if ref.BlockOwnerDeletion != nil {
		check.blockOwnerDeletion = *ref.BlockOwnerDeletion
	}
	return check
}

type pvIndex map[string]*corev1.PersistentVolume

func collectPVs(ctx context.Context, client kubernetes.Interface) (pvIndex, error) {
	pvs, err := client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	index := pvIndex{}
	for i := range pvs.Items {
		pv := pvs.Items[i].DeepCopy()
		index[pv.Name] = pv
	}
	return index, nil
}

func (i pvIndex) BoundPV(pvc *corev1.PersistentVolumeClaim) (*corev1.PersistentVolume, bool) {
	if pvc.Spec.VolumeName == "" {
		return nil, false
	}
	pv := i[pvc.Spec.VolumeName]
	if pv == nil {
		return nil, false
	}
	return pv.DeepCopy(), pvClaimRefMatches(pv, pvc)
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

func sortTargets(targets []targetPVC) {
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].namespace != targets[j].namespace {
			return targets[i].namespace < targets[j].namespace
		}
		if targets[i].name != targets[j].name {
			return targets[i].name < targets[j].name
		}
		return targets[i].owner.status < targets[j].owner.status
	})
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
		Namespace:               target.namespace,
		PVC:                     target.name,
		OwnerStatus:             target.owner.status,
		OwnerAPIVersion:         target.owner.apiVersion,
		OwnerKind:               target.owner.kind,
		OwnerNamespace:          target.owner.namespace,
		OwnerName:               target.owner.name,
		OwnerUID:                target.owner.uid,
		OwnerController:         target.owner.controller,
		OwnerBlockOwnerDeletion: target.owner.blockOwnerDeletion,
		OwnerRefCount:           target.ownerRefCount,
		Reason:                  target.owner.reason,
		PVClaimRefMatched:       target.pvClaimRefMatched,
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
		_, err := fmt.Fprintln(w, "no orphaned PVC candidates found")
		return err
	}

	if _, err := fmt.Fprintf(w, "found %d orphaned PVC candidate(s)\n", len(rows)); err != nil {
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
		r.OwnerStatus,
		r.Reason,
		r.PVCPhase,
		r.PVPhase,
		r.PVReclaimPolicy,
		fmt.Sprintf("%t", r.PVClaimRefMatched),
		r.PVCStorageClass,
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

func buildClients(kubeconfig string) (*clients, error) {
	config, err := buildRESTConfig(kubeconfig)
	if err != nil {
		return nil, err
	}

	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, err
	}

	return &clients{
		kubernetes: kubeClient,
		dynamic:    dynamicClient,
		mapper:     restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient)),
	}, nil
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
