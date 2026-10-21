package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestParseOptionsDefaultsToDryRunAndAllNodes(t *testing.T) {
	var output bytes.Buffer
	opts, err := parseOptions(nil, &output)
	if err != nil {
		t.Fatal(err)
	}
	if opts.confirm {
		t.Fatal("expected dry-run by default")
	}
	if opts.node != "" {
		t.Fatalf("expected all nodes by default, got %q", opts.node)
	}
	if opts.namespace != metav1.NamespaceAll {
		t.Fatalf("expected all namespaces by default, got %q", opts.namespace)
	}
}

func TestParseOptionsRejectsInvalidExcludedPod(t *testing.T) {
	_, err := parseOptions([]string{"--exclude-pod", "pod-only"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected invalid excluded Pod to fail")
	}
}

func TestDetectPodTokenUseReportsNonDefaultServiceAccountWithAutomount(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "prod"},
		Spec: corev1.PodSpec{
			ServiceAccountName: "api",
		},
	}

	finding := detectPodTokenUse(pod, nil, nil)

	if _, ok := finding.serviceAccounts["api"]; !ok {
		t.Fatalf("expected non-default ServiceAccount to be reported")
	}
	if _, ok := finding.tokenSources["effective automountServiceAccountToken=true"]; !ok {
		t.Fatalf("expected effective automount token source to be reported")
	}
}

func TestDetectPodTokenUseSkipsDisabledAutomount(t *testing.T) {
	automount := false
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "prod"},
		Spec: corev1.PodSpec{
			ServiceAccountName:           "api",
			AutomountServiceAccountToken: &automount,
		},
	}

	finding := detectPodTokenUse(pod, nil, nil)
	if len(finding.serviceAccounts) != 0 {
		t.Fatalf("expected no finding, got %v", sortedKeys(finding.serviceAccounts))
	}
}

func TestDetectPodTokenUseRespectsServiceAccountAutomount(t *testing.T) {
	automount := false
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "prod"},
		Spec:       corev1.PodSpec{ServiceAccountName: "api"},
	}

	finding := detectPodTokenUse(pod, map[types.NamespacedName]*bool{
		namespacedName("prod", "api"): &automount,
	}, nil)
	if len(finding.serviceAccounts) != 0 {
		t.Fatalf("expected ServiceAccount-level automount=false to suppress finding, got %v", sortedKeys(finding.serviceAccounts))
	}
}

func TestDetectPodTokenUseReportsProjectedTokenWhenAutomountDisabled(t *testing.T) {
	automount := false
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "prod"},
		Spec: corev1.PodSpec{
			ServiceAccountName:           "api",
			AutomountServiceAccountToken: &automount,
			Volumes: []corev1.Volume{{
				Name: "api-token",
				VolumeSource: corev1.VolumeSource{
					Projected: &corev1.ProjectedVolumeSource{
						Sources: []corev1.VolumeProjection{{
							ServiceAccountToken: &corev1.ServiceAccountTokenProjection{},
						}},
					},
				},
			}},
		},
	}

	finding := detectPodTokenUse(pod, nil, nil)
	if _, ok := finding.serviceAccounts["api"]; !ok {
		t.Fatal("expected projected token to be reported")
	}
}

func TestDetectPodTokenUseReportsLegacyTokenSecretReferences(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "prod"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name: "token",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: "builder-token"},
				},
			}},
			Containers: []corev1.Container{{
				Name: "worker",
				Env: []corev1.EnvVar{{
					Name: "TOKEN",
					ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "builder-token"},
						Key:                  "token",
					}},
				}},
			}},
		},
	}
	secrets := map[types.NamespacedName]string{
		namespacedName("prod", "builder-token"): "builder",
	}

	finding := detectPodTokenUse(pod, nil, secrets)
	if _, ok := finding.serviceAccounts["builder"]; !ok {
		t.Fatal("expected legacy token Secret to be reported")
	}
	if len(finding.tokenSources) != 2 {
		t.Fatalf("expected volume and env token sources, got %v", sortedKeys(finding.tokenSources))
	}
}

func TestFindMatchingPodsFiltersNodeStateAndExcludedPod(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "prod"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "disabled", Namespace: "prod"}, AutomountServiceAccountToken: boolPtr(false)},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "builder-token",
				Namespace: "prod",
				Annotations: map[string]string{
					corev1.ServiceAccountNameKey: "builder",
				},
			},
			Type: corev1.SecretTypeServiceAccountToken,
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "prod"},
			Spec:       corev1.PodSpec{NodeName: "worker-a", ServiceAccountName: "api"},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "builder-0", Namespace: "prod"},
			Spec: corev1.PodSpec{
				NodeName: "worker-a",
				Volumes: []corev1.Volume{{VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: "builder-token"},
				}}},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "api-other-node", Namespace: "prod"},
			Spec:       corev1.PodSpec{NodeName: "worker-b", ServiceAccountName: "api"},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "disabled-0", Namespace: "prod"},
			Spec:       corev1.PodSpec{NodeName: "worker-a", ServiceAccountName: "disabled"},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "finished-0", Namespace: "prod"},
			Spec:       corev1.PodSpec{NodeName: "worker-a", ServiceAccountName: "api"},
			Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "excluded-0", Namespace: "prod"},
			Spec:       corev1.PodSpec{NodeName: "worker-a", ServiceAccountName: "api"},
		},
	)

	targets, warnings, err := findMatchingPods(context.Background(), client, options{
		node:       "worker-a",
		excludePod: "prod/excluded-0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}

	if len(targets) != 2 {
		t.Fatalf("expected two targets, got %#v", targets)
	}
	if targets[0].name != "api-0" || targets[1].name != "builder-0" {
		t.Fatalf("expected sorted api-0 and builder-0, got %#v", targets)
	}
}

func TestRunWithClientDryRunDoesNotDelete(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "prod"}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "prod"},
			Spec:       corev1.PodSpec{NodeName: "worker-a", ServiceAccountName: "api"},
		},
	)
	var stdout, stderr strings.Builder

	err := runWithClient(context.Background(), client, options{}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "dry-run") || !strings.Contains(stdout.String(), "would-delete") {
		t.Fatalf("expected dry-run output, got %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr, got %q", stderr.String())
	}
	if _, err := client.CoreV1().Pods("prod").Get(context.Background(), "api-0", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected dry-run to preserve Pod, got %v", err)
	}
}

func TestRunWithClientConfirmDeletesMatchingPods(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "prod"}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "prod"},
			Spec:       corev1.PodSpec{NodeName: "worker-a", ServiceAccountName: "api"},
		},
	)
	var stdout, stderr strings.Builder

	err := runWithClient(context.Background(), client, options{confirm: true}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "status=deleted") {
		t.Fatalf("expected deletion output, got %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr, got %q", stderr.String())
	}
	if _, err := client.CoreV1().Pods("prod").Get(context.Background(), "api-0", metav1.GetOptions{}); err == nil {
		t.Fatal("expected matching Pod to be deleted")
	}
}

func boolPtr(value bool) *bool {
	return &value
}
