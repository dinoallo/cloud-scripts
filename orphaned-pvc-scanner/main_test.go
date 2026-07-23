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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

type staticOwnerResolver map[string]ownerCheck

func (r staticOwnerResolver) ResolveOwner(_ context.Context, pvc *corev1.PersistentVolumeClaim, ref metav1.OwnerReference) ownerCheck {
	check, ok := r[ref.Name]
	if !ok {
		check = ownerCheck{
			status: ownerNotFound,
			reason: "owner missing in test resolver",
		}
	}
	base := ownerCheckFromReference(pvc, ref)
	base.status = check.status
	base.namespace = check.namespace
	if base.namespace == "" && check.status != ownerNoReferences {
		base.namespace = pvc.Namespace
	}
	base.reason = check.reason
	return base
}

func TestClassifyPVCWithoutOwnerReferences(t *testing.T) {
	t.Parallel()

	owner, orphan := classifyPVC(context.Background(), pvc("data", "prod", nil), staticOwnerResolver{})
	if !orphan {
		t.Fatal("expected PVC without ownerReferences to be classified as orphan")
	}
	if owner.status != ownerNoReferences {
		t.Fatalf("expected status %s, got %s", ownerNoReferences, owner.status)
	}
}

func TestClassifyPVCSkipsWhenAnyOwnerExists(t *testing.T) {
	t.Parallel()

	pvc := pvc("data", "prod", []metav1.OwnerReference{
		ownerRef("missing-owner", "missing-uid"),
		ownerRef("live-owner", "live-uid"),
	})
	_, orphan := classifyPVC(context.Background(), pvc, staticOwnerResolver{
		"missing-owner": {
			status: ownerNotFound,
			reason: "owner is missing",
		},
		"live-owner": {
			status: ownerFound,
			reason: "owner exists",
		},
	})
	if orphan {
		t.Fatal("expected PVC to be skipped when at least one owner exists")
	}
}

func TestClassifyPVCSelectsLookupErrorBeforeMissingOwner(t *testing.T) {
	t.Parallel()

	pvc := pvc("data", "prod", []metav1.OwnerReference{
		ownerRef("missing-owner", "missing-uid"),
		ownerRef("unknown-owner", "unknown-uid"),
	})
	owner, orphan := classifyPVC(context.Background(), pvc, staticOwnerResolver{
		"missing-owner": {
			status: ownerNotFound,
			reason: "owner is missing",
		},
		"unknown-owner": {
			status: ownerLookupError,
			reason: "forbidden",
		},
	})
	if !orphan {
		t.Fatal("expected unresolved PVC to be reported")
	}
	if owner.status != ownerLookupError {
		t.Fatalf("expected status %s, got %s", ownerLookupError, owner.status)
	}
}

func TestCollectTargets(t *testing.T) {
	t.Parallel()

	storageClass := "fast"
	pvcUID := types.UID("pvc-uid")
	client := fake.NewSimpleClientset(
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "no-owner",
				Namespace:         "prod",
				UID:               pvcUID,
				CreationTimestamp: metav1.NewTime(time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)),
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: &storageClass,
				VolumeName:       "pv-no-owner",
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
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "has-owner",
				Namespace:       "prod",
				OwnerReferences: []metav1.OwnerReference{ownerRef("live-owner", "live-uid")},
			},
		},
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pv-no-owner",
			},
			Spec: corev1.PersistentVolumeSpec{
				ClaimRef: &corev1.ObjectReference{
					Namespace: "prod",
					Name:      "no-owner",
					UID:       pvcUID,
				},
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			},
			Status: corev1.PersistentVolumeStatus{
				Phase: corev1.VolumeBound,
			},
		},
	)

	targets, err := collectTargets(context.Background(), client, staticOwnerResolver{
		"live-owner": {
			status: ownerFound,
			reason: "owner exists",
		},
	}, options{namespace: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected one target, got %d: %#v", len(targets), targets)
	}
	if targets[0].name != "no-owner" {
		t.Fatalf("expected no-owner target, got %s", targets[0].name)
	}
	if targets[0].owner.status != ownerNoReferences {
		t.Fatalf("expected status %s, got %s", ownerNoReferences, targets[0].owner.status)
	}
	if targets[0].pv == nil || targets[0].pv.Name != "pv-no-owner" {
		t.Fatalf("expected bound PV pv-no-owner, got %#v", targets[0].pv)
	}
	if !targets[0].pvClaimRefMatched {
		t.Fatal("expected PV claimRef to match")
	}
}

func TestCollectTargetsSkipsNoOwnerPVCUsedByActivePod(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(
		pvc("data", "prod", nil),
		podUsingPVC("app-0", "prod", "data", corev1.PodRunning),
	)

	targets, err := collectTargets(context.Background(), client, staticOwnerResolver{}, options{namespace: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 0 {
		t.Fatalf("expected active pod usage to skip no-owner PVC, got %#v", targets)
	}
}

func TestCollectTargetsReportsNoOwnerPVCOnlyUsedByTerminalPod(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(
		pvc("data", "prod", nil),
		podUsingPVC("completed", "prod", "data", corev1.PodSucceeded),
	)

	targets, err := collectTargets(context.Background(), client, staticOwnerResolver{}, options{namespace: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected terminal pod usage not to hide no-owner PVC, got %#v", targets)
	}
	if targets[0].name != "data" {
		t.Fatalf("expected data target, got %s", targets[0].name)
	}
}

func TestCollectTargetsDoesNotSkipBrokenOwnerPVCUsedByActivePod(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(
		pvc("data", "prod", []metav1.OwnerReference{ownerRef("missing-owner", "missing-uid")}),
		podUsingPVC("app-0", "prod", "data", corev1.PodRunning),
	)

	targets, err := collectTargets(context.Background(), client, staticOwnerResolver{}, options{namespace: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected broken owner PVC to still be reported, got %#v", targets)
	}
	if targets[0].owner.status != ownerNotFound {
		t.Fatalf("expected status %s, got %s", ownerNotFound, targets[0].owner.status)
	}
}

func TestWriteOutputCSV(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
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
	assertField(t, row, "ownerStatus", ownerNotFound)
	assertField(t, row, "ownerAPIVersion", "apps/v1")
	assertField(t, row, "ownerKind", "StatefulSet")
	assertField(t, row, "ownerNamespace", "prod")
	assertField(t, row, "ownerName", "mysql")
	assertField(t, row, "ownerUID", "owner-uid")
	assertField(t, row, "ownerController", "true")
	assertField(t, row, "ownerBlockOwnerDeletion", "false")
	assertField(t, row, "ownerRefCount", "1")
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

	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := writeOutput(&buf, []targetPVC{outputTarget(now)}, outputJSONL, now); err != nil {
		t.Fatal(err)
	}

	var row outputRow
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &row); err != nil {
		t.Fatal(err)
	}
	if row.OwnerStatus != ownerNotFound {
		t.Fatalf("expected ownerStatus %s, got %s", ownerNotFound, row.OwnerStatus)
	}
	if row.OwnerRefCount != 1 {
		t.Fatalf("expected ownerRefCount 1, got %d", row.OwnerRefCount)
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

func pvc(name, namespace string, refs []metav1.OwnerReference) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			OwnerReferences: refs,
		},
	}
}

func ownerRef(name, uid string) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "StatefulSet",
		Name:       name,
		UID:        types.UID(uid),
		Controller: &controller,
	}
}

func podUsingPVC(name, namespace, claimName string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
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
							ClaimName: claimName,
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
	pvcUID := types.UID("pvc-uid")
	return targetPVC{
		namespace: "prod",
		name:      "data-mysql-0",
		owner: ownerCheck{
			status:     ownerNotFound,
			apiVersion: "apps/v1",
			kind:       "StatefulSet",
			namespace:  "prod",
			name:       "mysql",
			uid:        "owner-uid",
			controller: true,
			reason:     "owner object was not found in the PVC namespace",
		},
		ownerRefCount: 1,
		pvc: &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "data-mysql-0",
				Namespace:         "prod",
				UID:               pvcUID,
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
