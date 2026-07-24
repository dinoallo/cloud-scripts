package main

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCollectPVCOnlyTargets(t *testing.T) {
	t.Parallel()

	const namespace = "ns-test"
	pvcs := []corev1.PersistentVolumeClaim{
		pvc("data-leftover-0", namespace, nil, storeAnnotations("/data", "10")),
		pvc("data-no-annotations-0", namespace, nil, nil),
		pvc("data-empty-value-0", namespace, nil, storeAnnotations("/data", "")),
		pvc("data-owned-app-0", namespace, map[string]string{
			legacyAppLabelKey: "app-a",
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

	if len(targets) != 1 {
		t.Fatalf("expected exactly one PVC-only target, got %d: %#v", len(targets), targets)
	}
	if targets[0].name != "data-leftover-0" {
		t.Fatalf("expected data-leftover-0, got %s", targets[0].name)
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

func TestCollectTargetsSkipsInUsePVC(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(
		pvcPtr("data-leftover-0", "ns-test", nil, storeAnnotations("/data", "10")),
		pvcPtr("data-in-use-0", "ns-test", nil, storeAnnotations("/data", "10")),
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "consumer",
				Namespace: "ns-test",
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

	targets, err := collectTargets(context.Background(), client, options{
		namespace: "ns-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected one target, got %d: %#v", len(targets), targets)
	}
	if targets[0].name != "data-leftover-0" {
		t.Fatalf("expected data-leftover-0, got %s", targets[0].name)
	}
}

func TestRejectDeprecatedScopeFlags(t *testing.T) {
	t.Parallel()

	if err := rejectDeprecatedScopeFlags(options{}); err != nil {
		t.Fatalf("expected empty options to be accepted: %v", err)
	}
	if err := rejectDeprecatedScopeFlags(options{instance: "app-a"}); err == nil {
		t.Fatal("expected --instance to be rejected")
	}
	if err := rejectDeprecatedScopeFlags(options{discoverOrphans: true}); err == nil {
		t.Fatal("expected --discover-orphans to be rejected")
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
