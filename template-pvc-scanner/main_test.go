package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCollectPVCOnlyTargets(t *testing.T) {
	t.Parallel()

	const namespace = "ns-test"
	pvcs := []corev1.PersistentVolumeClaim{
		pvc("data-leftover-0", namespace, nil, storeAnnotations("/data", "10")),
		pvc("data-no-annotations-0", namespace, nil, nil),
		pvc("data-empty-path-0", namespace, nil, storeAnnotations("", "10")),
		pvc("data-leftover-with-app-label-0", namespace, map[string]string{
			"app": "app-a",
		}, storeAnnotations("/data", "10")),
		pvc("data-owned-template-0", namespace, map[string]string{
			templateDeployKey: "instance-a",
		}, storeAnnotations("/data", "10")),
		pvc("data-owned-applaunchpad-0", namespace, map[string]string{
			appDeployKey: "app-a",
		}, storeAnnotations("/data", "10")),
		pvc("data-in-use-0", namespace, nil, storeAnnotations("/data", "10")),
	}

	targets := collectPVCOnlyTargets(pvcs, map[string]struct{}{
		namespacedName(namespace, "data-in-use-0"): {},
	})

	if len(targets) != 2 {
		t.Fatalf("expected exactly two PVC-only targets, got %d: %#v", len(targets), targets)
	}
	if targets[0].name != "data-leftover-0" {
		t.Fatalf("expected data-leftover-0, got %s", targets[0].name)
	}
	if targets[1].name != "data-leftover-with-app-label-0" {
		t.Fatalf("expected data-leftover-with-app-label-0, got %s", targets[1].name)
	}
	if targets[0].path != "/data" {
		t.Fatalf("expected path /data, got %s", targets[0].path)
	}
	if targets[0].value != "10" {
		t.Fatalf("expected value 10, got %s", targets[0].value)
	}
}

func TestCollectUsedPVCsSkipsTerminalPods(t *testing.T) {
	t.Parallel()

	pods := []corev1.Pod{
		podUsingPVC("running", "ns-a", "data-running", corev1.PodRunning),
		podUsingPVC("pending", "ns-a", "data-pending", corev1.PodPending),
		podUsingPVC("succeeded", "ns-a", "data-succeeded", corev1.PodSucceeded),
		podUsingPVC("failed", "ns-a", "data-failed", corev1.PodFailed),
	}

	used := collectUsedPVCs(pods)
	for _, name := range []string{"data-running", "data-pending"} {
		if _, ok := used[namespacedName("ns-a", name)]; !ok {
			t.Fatalf("expected %s to be marked as used", name)
		}
	}
	for _, name := range []string{"data-succeeded", "data-failed"} {
		if _, ok := used[namespacedName("ns-a", name)]; ok {
			t.Fatalf("expected %s to be ignored", name)
		}
	}
}

func TestCollectTargetsAllNamespacesSkipsInUsePVC(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(
		pvcPtr("data-leftover-0", "ns-a", nil, storeAnnotations("/data", "10")),
		pvcPtr("data-in-use-0", "ns-b", nil, storeAnnotations("/data", "10")),
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "consumer",
				Namespace: "ns-b",
			},
			Spec: corev1.PodSpec{
				Volumes: []corev1.Volume{
					{
						Name: "data",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: "data-in-use-0",
							},
						},
					},
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
			},
		},
	)

	targets, err := collectTargets(context.Background(), client, options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected one target, got %d: %#v", len(targets), targets)
	}
	if targets[0].namespace != "ns-a" {
		t.Fatalf("expected ns-a target, got %s", targets[0].namespace)
	}
}

func TestWriteOutputCSV(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := writeOutput(&buf, []targetPVC{outputTarget(now)}, outputCSV, now); err != nil {
		t.Fatal(err)
	}

	records, err := csv.NewReader(bytes.NewReader(buf.Bytes())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("expected header and one row, got %d records: %#v", len(records), records)
	}

	row := recordByHeader(t, records[0], records[1])
	assertField(t, row, "namespace", "prod")
	assertField(t, row, "pvc", "data-mysql-0")
	assertField(t, row, "path", "/data")
	assertField(t, row, "value", "10")
	assertField(t, row, "reason", "test reason")
	assertField(t, row, "pvcPhase", "Bound")
	assertField(t, row, "pvcStorageClass", "fast")
	assertField(t, row, "pvcSize", "10Gi")
	assertField(t, row, "pvcAge", "2d1h")
}

func TestWriteOutputJSONL(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := writeOutput(&buf, []targetPVC{outputTarget(now)}, outputJSONL, now); err != nil {
		t.Fatal(err)
	}

	var row outputRow
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &row); err != nil {
		t.Fatal(err)
	}

	if row.Namespace != "prod" {
		t.Fatalf("expected namespace prod, got %s", row.Namespace)
	}
	if row.PVC != "data-mysql-0" {
		t.Fatalf("expected PVC data-mysql-0, got %s", row.PVC)
	}
	if row.Path != "/data" {
		t.Fatalf("expected path /data, got %s", row.Path)
	}
	if row.PVCAge != "2d1h" {
		t.Fatalf("expected pvcAge 2d1h, got %s", row.PVCAge)
	}
}

func TestValidateOutputFormat(t *testing.T) {
	t.Parallel()

	for _, format := range []string{outputTable, outputCSV, outputJSONL} {
		if err := validateOutputFormat(format); err != nil {
			t.Fatalf("expected %s to be valid: %v", format, err)
		}
	}
	if err := validateOutputFormat("yaml"); err == nil {
		t.Fatal("expected unsupported output format to fail")
	}
}

func pvc(name, namespace string, labels map[string]string, annotations map[string]string) corev1.PersistentVolumeClaim {
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
		},
	}
}

func pvcPtr(name, namespace string, labels map[string]string, annotations map[string]string) *corev1.PersistentVolumeClaim {
	item := pvc(name, namespace, labels, annotations)
	return &item
}

func storeAnnotations(path, value string) map[string]string {
	return map[string]string{
		pathAnnotationKey:  path,
		valueAnnotationKey: value,
	}
}

func podUsingPVC(name, namespace, pvcName string, phase corev1.PodPhase) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: phase,
		},
	}
}

func outputTarget(now time.Time) targetPVC {
	storageClass := "fast"
	return targetPVC{
		namespace: "prod",
		name:      "data-mysql-0",
		path:      "/data",
		value:     "10",
		reason:    "test reason",
		pvc: &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "data-mysql-0",
				Namespace:         "prod",
				CreationTimestamp: metav1.NewTime(now.Add(-49 * time.Hour)),
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: &storageClass,
				VolumeName:       "pv-data-mysql-0",
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("10Gi"),
					},
				},
			},
			Status: corev1.PersistentVolumeClaimStatus{
				Phase: corev1.ClaimBound,
			},
		},
	}
}

func recordByHeader(t *testing.T, headers, values []string) map[string]string {
	t.Helper()
	if len(headers) != len(outputHeaders) {
		t.Fatalf("expected %d headers, got %d: %#v", len(outputHeaders), len(headers), headers)
	}
	if len(values) != len(headers) {
		t.Fatalf("expected %d values, got %d: %#v", len(headers), len(values), values)
	}

	row := map[string]string{}
	for i := range headers {
		if headers[i] != outputHeaders[i] {
			t.Fatalf("expected header %d to be %s, got %s", i, outputHeaders[i], headers[i])
		}
		row[headers[i]] = values[i]
	}
	return row
}

func assertField(t *testing.T, row map[string]string, field, want string) {
	t.Helper()
	if got := row[field]; got != want {
		t.Fatalf("expected %s=%q, got %q", field, want, got)
	}
}
