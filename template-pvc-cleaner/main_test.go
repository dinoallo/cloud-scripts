package main

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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

func pvc(name, namespace string, labels map[string]string) corev1.PersistentVolumeClaim {
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
	}
}
