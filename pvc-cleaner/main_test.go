package main

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestParseTargetsMinimalCSV(t *testing.T) {
	t.Parallel()

	targets, err := parseTargets(strings.NewReader("namespace,pvc\nns-a,data-a\nns-b,data-b\n"))
	if err != nil {
		t.Fatal(err)
	}
	assertTargets(t, targets, []targetPVC{
		{namespace: "ns-a", name: "data-a"},
		{namespace: "ns-b", name: "data-b"},
	})
}

func TestParseTargetsScannerCSV(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		"namespace,pvc,path,value,reason,pvcPhase,pvcStorageClass,pvcSize,pvcAge",
		"prod,data-mysql-0,/data,10,test reason,Bound,fast,10Gi,2d1h",
		"",
	}, "\n")
	targets, err := parseTargets(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	assertTargets(t, targets, []targetPVC{
		{namespace: "prod", name: "data-mysql-0"},
	})
}

func TestParseTargetsSlashRecordsAndDeduplicates(t *testing.T) {
	t.Parallel()

	targets, err := parseTargets(strings.NewReader("# reviewed targets\nns-a/data-a\nns-a/data-a\nns-b/data-b\n"))
	if err != nil {
		t.Fatal(err)
	}
	assertTargets(t, targets, []targetPVC{
		{namespace: "ns-a", name: "data-a"},
		{namespace: "ns-b", name: "data-b"},
	})
}

func TestParseTargetsRejectsMissingPVCColumn(t *testing.T) {
	t.Parallel()

	_, err := parseTargets(strings.NewReader("namespace,path\nns-a,/data\n"))
	if err == nil {
		t.Fatal("expected missing pvc column to fail")
	}
}

func TestRunConfirmDeletesListedPVCs(t *testing.T) {
	t.Parallel()

	storageClass := "fast"
	client := fake.NewSimpleClientset(
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "data-a",
				Namespace: "ns-a",
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: &storageClass,
			},
			Status: corev1.PersistentVolumeClaimStatus{
				Phase: corev1.ClaimBound,
			},
		},
	)

	targets := []targetPVC{{namespace: "ns-a", name: "data-a"}}
	if err := deleteTargets(context.Background(), client, targets, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}

	_, err := client.CoreV1().PersistentVolumeClaims("ns-a").Get(context.Background(), "data-a", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected PVC to be deleted, got %v", err)
	}
}

func TestRunConfirmSkipsMissingPVCs(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	targets := []targetPVC{{namespace: "ns-a", name: "missing"}}
	if err := deleteTargets(context.Background(), client, targets, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestInspectPVCReportsPresentAndNotFound(t *testing.T) {
	t.Parallel()

	storageClass := "fast"
	client := fake.NewSimpleClientset(
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "data-a",
				Namespace: "ns-a",
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: &storageClass,
			},
			Status: corev1.PersistentVolumeClaimStatus{
				Phase: corev1.ClaimBound,
			},
		},
	)

	status, err := inspectPVC(context.Background(), client, targetPVC{namespace: "ns-a", name: "data-a"})
	if err != nil {
		t.Fatal(err)
	}
	if !status.found || status.phase != "Bound" || status.storageClass != "fast" {
		t.Fatalf("unexpected status: %#v", status)
	}

	status, err = inspectPVC(context.Background(), client, targetPVC{namespace: "ns-a", name: "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if status.found {
		t.Fatalf("expected missing PVC to report not found: %#v", status)
	}
}

func deleteTargets(ctx context.Context, client *fake.Clientset, targets []targetPVC, wait time.Duration) error {
	for _, target := range targets {
		status, err := inspectPVC(ctx, client, target)
		if err != nil {
			return err
		}
		if !status.found {
			continue
		}
		if err := deleteTarget(ctx, client, options{wait: wait}, target); err != nil {
			return err
		}
	}
	return nil
}

func assertTargets(t *testing.T, got, want []targetPVC) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("expected %d targets, got %d: %#v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected target %d to be %#v, got %#v", i, want[i], got[i])
		}
	}
}

func TestValidateTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		target  targetPVC
		wantErr bool
	}{
		{name: "valid", target: targetPVC{namespace: "ns-a", name: "data-a"}},
		{name: "missing namespace", target: targetPVC{name: "data-a"}, wantErr: true},
		{name: "missing pvc", target: targetPVC{namespace: "ns-a"}, wantErr: true},
		{name: "slash namespace", target: targetPVC{namespace: "ns/a", name: "data-a"}, wantErr: true},
		{name: "slash pvc", target: targetPVC{namespace: "ns-a", name: "data/a"}, wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateTarget(tt.target)
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTargetFromSlashRecordRejectsInvalidValue(t *testing.T) {
	t.Parallel()

	_, err := targetFromSlashRecord("not-a-target")
	if err == nil {
		t.Fatal("expected invalid slash target to fail")
	}
}
