package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestInferClaimTemplateFromLegacyAppPVC(t *testing.T) {
	t.Parallel()

	claimTemplate, ok := inferClaimTemplateFromLegacyAppPVC("data-mysql-primary-0", "mysql-primary")
	if !ok {
		t.Fatal("expected PVC name to match")
	}
	if claimTemplate != "data" {
		t.Fatalf("expected claim template data, got %q", claimTemplate)
	}

	if _, ok := inferClaimTemplateFromLegacyAppPVC("data-mysql-primary-0", "redis"); ok {
		t.Fatal("expected mismatched legacy app label to be rejected")
	}
}

func TestCollectDiscoveredOrphanTargets(t *testing.T) {
	t.Parallel()

	const namespace = "ns-test"
	client := fake.NewSimpleClientset(&appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "live-mysql",
			Namespace: namespace,
		},
	})

	pvcs := []corev1.PersistentVolumeClaim{
		pvc("data-orphan-mysql-0", namespace, map[string]string{
			legacyAppLabelKey: "orphan-mysql",
		}),
		pvc("data-labeled-mysql-0", namespace, map[string]string{
			legacyAppLabelKey: "labeled-mysql",
			templateDeployKey: "instance-a",
		}),
		pvc("data-other-mysql-0", namespace, map[string]string{
			legacyAppLabelKey: "unrelated-app",
		}),
		pvc("data-live-mysql-0", namespace, map[string]string{
			legacyAppLabelKey: "live-mysql",
		}),
	}

	targets, err := collectDiscoveredOrphanTargets(context.Background(), client, options{
		namespace:       namespace,
		instance:        "instance-a",
		discoverOrphans: true,
	}, pvcs)
	if err != nil {
		t.Fatal(err)
	}

	if len(targets) != 1 {
		t.Fatalf("expected exactly one discovered target, got %d: %#v", len(targets), targets)
	}
	if targets[0].name != "data-orphan-mysql-0" {
		t.Fatalf("expected data-orphan-mysql-0, got %s", targets[0].name)
	}
	if targets[0].statefulSet != "orphan-mysql" {
		t.Fatalf("expected inferred StatefulSet orphan-mysql, got %s", targets[0].statefulSet)
	}
	if targets[0].claimTemplate != "data" {
		t.Fatalf("expected inferred claim template data, got %s", targets[0].claimTemplate)
	}
}

func TestCollectDiscoveredOrphanTargetsAllNamespaces(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(&appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shared-mysql",
			Namespace: "ns-a",
		},
	})

	pvcs := []corev1.PersistentVolumeClaim{
		pvc("data-shared-mysql-0", "ns-a", map[string]string{
			legacyAppLabelKey: "shared-mysql",
		}),
		pvc("data-shared-mysql-0", "ns-b", map[string]string{
			legacyAppLabelKey: "shared-mysql",
		}),
	}

	targets, err := collectDiscoveredOrphanTargets(context.Background(), client, options{
		instance:        "instance-a",
		discoverOrphans: true,
	}, pvcs)
	if err != nil {
		t.Fatal(err)
	}

	if len(targets) != 1 {
		t.Fatalf("expected exactly one discovered target, got %d: %#v", len(targets), targets)
	}
	if targets[0].namespace != "ns-b" {
		t.Fatalf("expected ns-b target, got namespace %s", targets[0].namespace)
	}
}

func TestCollectLiveStatefulSetTargetsAllNamespaces(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(&appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shared-mysql",
			Namespace: "ns-a",
			Labels: map[string]string{
				templateDeployKey: "instance-a",
			},
		},
		Spec: appsv1.StatefulSetSpec{
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "data",
					},
				},
			},
		},
	})

	pvcs := []corev1.PersistentVolumeClaim{
		pvc("data-shared-mysql-0", "ns-a", nil),
		pvc("data-shared-mysql-0", "ns-b", nil),
	}

	targets, err := collectLiveStatefulSetTargets(context.Background(), client, options{
		instance: "instance-a",
	}, pvcs)
	if err != nil {
		t.Fatal(err)
	}

	if len(targets) != 1 {
		t.Fatalf("expected exactly one live target, got %d: %#v", len(targets), targets)
	}
	if targets[0].namespace != "ns-a" {
		t.Fatalf("expected ns-a target, got namespace %s", targets[0].namespace)
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
	assertField(t, row, "pv", "pv-data-mysql-0")
	assertField(t, row, "statefulset", "mysql")
	assertField(t, row, "claimTemplate", "data")
	assertField(t, row, "reason", "test reason")
	assertField(t, row, "pvcPhase", "Bound")
	assertField(t, row, "pvPhase", "Bound")
	assertField(t, row, "pvReclaimPolicy", "Retain")
	assertField(t, row, "pvClaimRefMatched", "true")
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
	if !row.PVClaimRefMatched {
		t.Fatal("expected pvClaimRefMatched to be true")
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

func pvc(name, namespace string, labels map[string]string) corev1.PersistentVolumeClaim {
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
	}
}

func outputTarget(now time.Time) targetPVC {
	storageClass := "fast"
	return targetPVC{
		namespace:     "prod",
		name:          "data-mysql-0",
		statefulSet:   "mysql",
		claimTemplate: "data",
		reason:        "test reason",
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
		pv: &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pv-data-mysql-0",
			},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			},
			Status: corev1.PersistentVolumeStatus{
				Phase: corev1.VolumeBound,
			},
		},
		pvClaimRefMatched: true,
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
