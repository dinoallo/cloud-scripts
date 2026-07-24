package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
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

type options struct {
	kubeconfig string
	input      string
	confirm    bool
	wait       time.Duration
}

type targetPVC struct {
	namespace string
	name      string
}

type inputColumns struct {
	hasHeader      bool
	namespaceIndex int
	pvcIndex       int
}

type pvcStatus struct {
	found        bool
	phase        string
	storageClass string
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
	flag.StringVar(&opts.input, "input", "", "CSV file containing namespace,pvc targets; use - for stdin")
	flag.BoolVar(&opts.confirm, "confirm", false, "delete listed PVCs; without this flag the program only prints a dry-run plan")
	flag.DurationVar(&opts.wait, "wait", 2*time.Minute, "maximum time to wait for each PVC deletion")
	flag.Parse()
	return opts
}

func run(ctx context.Context, opts options) error {
	if strings.TrimSpace(opts.input) == "" {
		return errors.New("--input is required")
	}

	targets, err := loadTargets(opts.input)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		fmt.Println("no PVC targets found in input")
		return nil
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].namespace != targets[j].namespace {
			return targets[i].namespace < targets[j].namespace
		}
		return targets[i].name < targets[j].name
	})

	client, err := buildClient(opts.kubeconfig)
	if err != nil {
		return err
	}

	if opts.confirm {
		fmt.Printf("deleting %d PVC target(s) from input\n", len(targets))
	} else {
		fmt.Printf("dry-run: found %d PVC target(s) from input; pass --confirm to delete\n", len(targets))
	}

	for _, target := range targets {
		status, err := inspectPVC(ctx, client, target)
		if err != nil {
			return err
		}
		printTarget(target, status)
		if !opts.confirm || !status.found {
			continue
		}
		if err := deleteTarget(ctx, client, opts, target); err != nil {
			return err
		}
	}

	return nil
}

func loadTargets(path string) ([]targetPVC, error) {
	if path == "-" {
		return parseTargets(os.Stdin)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	return parseTargets(file)
}

func parseTargets(r io.Reader) ([]targetPVC, error) {
	reader := csv.NewReader(r)
	reader.Comment = '#'
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true

	var columns *inputColumns
	var targets []targetPVC
	seen := map[string]struct{}{}
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		record = trimRecord(record)
		if emptyRecord(record) {
			continue
		}

		if columns == nil {
			if detected, ok := detectHeader(record); ok {
				columns = &detected
				continue
			}
			if looksLikeHeader(record) {
				return nil, errors.New("input header must include namespace and pvc columns")
			}
			columns = &inputColumns{
				namespaceIndex: 0,
				pvcIndex:       1,
			}
		}

		target, err := targetFromRecord(record, *columns)
		if err != nil {
			return nil, err
		}
		key := namespacedName(target.namespace, target.name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		targets = append(targets, target)
	}

	return targets, nil
}

func detectHeader(record []string) (inputColumns, bool) {
	columns := inputColumns{
		hasHeader:      true,
		namespaceIndex: -1,
		pvcIndex:       -1,
	}
	for i, field := range record {
		switch normalizeHeader(field) {
		case "namespace", "ns":
			columns.namespaceIndex = i
		case "pvc", "claim", "persistentvolumeclaim", "persistent_volume_claim":
			columns.pvcIndex = i
		}
	}
	return columns, columns.namespaceIndex >= 0 && columns.pvcIndex >= 0
}

func looksLikeHeader(record []string) bool {
	for _, field := range record {
		switch normalizeHeader(field) {
		case "namespace", "ns", "pvc", "claim", "persistentvolumeclaim", "persistent_volume_claim":
			return true
		}
	}
	return false
}

func targetFromRecord(record []string, columns inputColumns) (targetPVC, error) {
	if !columns.hasHeader && len(record) == 1 {
		return targetFromSlashRecord(record[0])
	}
	if columns.namespaceIndex >= len(record) || columns.pvcIndex >= len(record) {
		return targetPVC{}, fmt.Errorf("record must include namespace and pvc fields: %v", record)
	}

	target := targetPVC{
		namespace: record[columns.namespaceIndex],
		name:      record[columns.pvcIndex],
	}
	if err := validateTarget(target); err != nil {
		return targetPVC{}, err
	}
	return target, nil
}

func targetFromSlashRecord(value string) (targetPVC, error) {
	namespace, name, ok := strings.Cut(value, "/")
	if !ok {
		return targetPVC{}, fmt.Errorf("record must be namespace,pvc or namespace/pvc: %q", value)
	}
	target := targetPVC{
		namespace: strings.TrimSpace(namespace),
		name:      strings.TrimSpace(name),
	}
	if err := validateTarget(target); err != nil {
		return targetPVC{}, err
	}
	return target, nil
}

func validateTarget(target targetPVC) error {
	if target.namespace == "" {
		return errors.New("namespace is required for every PVC target")
	}
	if target.name == "" {
		return errors.New("pvc is required for every PVC target")
	}
	if strings.Contains(target.namespace, "/") || strings.Contains(target.name, "/") {
		return fmt.Errorf("invalid PVC target %q", namespacedName(target.namespace, target.name))
	}
	return nil
}

func trimRecord(record []string) []string {
	trimmed := make([]string, len(record))
	for i, field := range record {
		trimmed[i] = strings.TrimSpace(field)
	}
	return trimmed
}

func emptyRecord(record []string) bool {
	for _, field := range record {
		if field != "" {
			return false
		}
	}
	return true
}

func normalizeHeader(header string) string {
	header = strings.ToLower(strings.TrimSpace(header))
	header = strings.ReplaceAll(header, "-", "_")
	return header
}

func inspectPVC(ctx context.Context, client kubernetes.Interface, target targetPVC) (pvcStatus, error) {
	pvc, err := client.CoreV1().PersistentVolumeClaims(target.namespace).Get(ctx, target.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return pvcStatus{}, nil
	}
	if err != nil {
		return pvcStatus{}, err
	}
	return pvcStatus{
		found:        true,
		phase:        string(pvc.Status.Phase),
		storageClass: storageClassName(pvc),
	}, nil
}

func deleteTarget(ctx context.Context, client kubernetes.Interface, opts options, target targetPVC) error {
	err := client.CoreV1().PersistentVolumeClaims(target.namespace).Delete(ctx, target.name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return waitForPVCGone(ctx, client, target.namespace, target.name, opts.wait)
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

func printTarget(target targetPVC, status pvcStatus) {
	if !status.found {
		fmt.Printf("- pvc=%s/%s status=not-found\n", target.namespace, target.name)
		return
	}
	fmt.Printf("- pvc=%s/%s status=present phase=%s storageClass=%s\n",
		target.namespace,
		target.name,
		status.phase,
		status.storageClass,
	)
}

func storageClassName(pvc *corev1.PersistentVolumeClaim) string {
	if pvc.Spec.StorageClassName == nil {
		return ""
	}
	return *pvc.Spec.StorageClassName
}

func namespacedName(namespace, name string) string {
	return namespace + "/" + name
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
