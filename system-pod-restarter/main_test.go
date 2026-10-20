package main

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestFindRestartTargetsSkipsExcludedNamespacesAndNonTargetControllers(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns-platform"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default", UID: "deployment-api"}},
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "api-7c8d",
				Namespace: "default",
				UID:       "replicaset-api",
				OwnerReferences: []metav1.OwnerReference{
					controllerOwner("Deployment", "api", "deployment-api"),
				},
			},
		},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "node-agent", Namespace: "default", UID: "daemonset-agent"}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "database", Namespace: "default", UID: "statefulset-database"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "ignored-api", Namespace: "ns-platform", UID: "deployment-ignored"}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "api-7c8d-abc",
				Namespace:       "default",
				OwnerReferences: []metav1.OwnerReference{controllerOwner("ReplicaSet", "api-7c8d", "replicaset-api")},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "node-agent-abc",
				Namespace:       "default",
				OwnerReferences: []metav1.OwnerReference{controllerOwner("DaemonSet", "node-agent", "daemonset-agent")},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "database-0",
				Namespace:       "default",
				OwnerReferences: []metav1.OwnerReference{controllerOwner("StatefulSet", "database", "statefulset-database")},
			},
		},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "standalone", Namespace: "default"}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "ignored-api-abc",
				Namespace:       "ns-platform",
				OwnerReferences: []metav1.OwnerReference{controllerOwner("Deployment", "ignored-api", "deployment-ignored")},
			},
		},
	)

	targets, err := findRestartTargets(context.Background(), client, "ns-")
	if err != nil {
		t.Fatal(err)
	}

	want := []podTarget{
		{namespace: "default", name: "api-7c8d-abc", ownerKind: "Deployment", ownerName: "api"},
		{namespace: "default", name: "node-agent-abc", ownerKind: "DaemonSet", ownerName: "node-agent"},
	}
	if len(targets) != len(want) {
		t.Fatalf("expected %d targets, got %d: %#v", len(want), len(targets), targets)
	}
	for i := range want {
		if targets[i] != want[i] {
			t.Fatalf("expected target %d to be %#v, got %#v", i, want[i], targets[i])
		}
	}
}

func TestFindRestartTargetsRejectsStaleReplicaSetOwner(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default", UID: "deployment-current"}},
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "api-old",
				Namespace: "default",
				UID:       "replicaset-old",
				OwnerReferences: []metav1.OwnerReference{
					controllerOwner("Deployment", "api", "deployment-old"),
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "api-old-abc",
				Namespace:       "default",
				OwnerReferences: []metav1.OwnerReference{controllerOwner("ReplicaSet", "api-old", "replicaset-old")},
			},
		},
	)

	targets, err := findRestartTargets(context.Background(), client, "ns-")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 0 {
		t.Fatalf("expected no targets for stale ReplicaSet, got %#v", targets)
	}
}

func TestRunWithClientDryRunDoesNotDelete(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "node-agent", Namespace: "default", UID: "daemonset-agent"}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "node-agent-abc",
				Namespace:       "default",
				OwnerReferences: []metav1.OwnerReference{controllerOwner("DaemonSet", "node-agent", "daemonset-agent")},
			},
		},
	)
	var stdout strings.Builder
	var stderr strings.Builder

	err := runWithClient(context.Background(), client, options{excludedNamespacePrefix: "ns-"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "dry-run") || !strings.Contains(stdout.String(), "would-delete") {
		t.Fatalf("expected dry-run output, got %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr, got %q", stderr.String())
	}

	_, err = client.CoreV1().Pods("default").Get(context.Background(), "node-agent-abc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected dry-run to preserve Pod, got %v", err)
	}
}

func TestRunWithClientConfirmDeletesTargets(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "node-agent", Namespace: "default", UID: "daemonset-agent"}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "node-agent-abc",
				Namespace:       "default",
				OwnerReferences: []metav1.OwnerReference{controllerOwner("DaemonSet", "node-agent", "daemonset-agent")},
			},
		},
	)
	var stdout strings.Builder
	var stderr strings.Builder

	err := runWithClient(context.Background(), client, options{excludedNamespacePrefix: "ns-", confirm: true}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "status=deleted") {
		t.Fatalf("expected deletion output, got %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr, got %q", stderr.String())
	}

	_, err = client.CoreV1().Pods("default").Get(context.Background(), "node-agent-abc", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected Pod to be deleted, got %v", err)
	}
}

func TestParseOptionsDefaultsToNsPrefixAndDryRun(t *testing.T) {
	var output strings.Builder
	opts, err := parseOptions(nil, &output)
	if err != nil {
		t.Fatal(err)
	}
	if opts.excludedNamespacePrefix != "ns-" {
		t.Fatalf("expected ns- as default excluded prefix, got %q", opts.excludedNamespacePrefix)
	}
	if opts.confirm {
		t.Fatal("expected dry-run by default")
	}
}

func TestParseOptionsRejectsEmptyExcludedPrefix(t *testing.T) {
	var output strings.Builder
	_, err := parseOptions([]string{"--exclude-namespace-prefix", ""}, &output)
	if err == nil {
		t.Fatal("expected empty excluded prefix to fail")
	}
}

func controllerOwner(kind, name string, uid types.UID) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       kind,
		Name:       name,
		UID:        uid,
		Controller: &controller,
	}
}
