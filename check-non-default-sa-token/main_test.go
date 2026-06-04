package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestRunPrintsHelpWithoutError(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	if err := run(context.Background(), []string{"--help"}, &stdout, &stderr); err != nil {
		t.Fatalf("expected help to return no error, got %v", err)
	}
	if stdout.Len() == 0 {
		t.Fatalf("expected help text on stdout")
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr, got %q", stderr.String())
	}
}

func TestParseOptionsDefaultsToCSVOutput(t *testing.T) {
	opts, err := parseOptions(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseOptions returned error: %v", err)
	}
	if opts.output != "csv" {
		t.Fatalf("expected csv output by default, got %q", opts.output)
	}
}

func TestWriteCSVOutputsDefaultColumns(t *testing.T) {
	result := scanResult{
		Items: []applicationFinding{
			{
				Namespace:       "prod",
				OwnerKind:       "ReplicaSet",
				OwnerName:       "api-775d7f5b7d",
				ServiceAccounts: []string{"api", "builder"},
			},
		},
	}
	var output bytes.Buffer

	if err := writeResult(&output, result, options{output: "csv"}); err != nil {
		t.Fatalf("writeResult returned error: %v", err)
	}

	want := "namespace,ownerKind,ownerName,serviceAccounts\nprod,ReplicaSet,api-775d7f5b7d,\"api,builder\"\n"
	if output.String() != want {
		t.Fatalf("unexpected csv output:\nwant %q\ngot  %q", want, output.String())
	}
}

func TestWriteCSVOutputsHeaderOnlyWhenNoFindings(t *testing.T) {
	var output bytes.Buffer

	if err := writeResult(&output, scanResult{}, options{output: "csv"}); err != nil {
		t.Fatalf("writeResult returned error: %v", err)
	}

	if got := output.String(); got != "namespace,ownerKind,ownerName,serviceAccounts\n" {
		t.Fatalf("unexpected csv output %q", got)
	}
}

func TestWriteResultRejectsUnsupportedOutput(t *testing.T) {
	err := writeResult(&strings.Builder{}, scanResult{}, options{output: "yaml"})
	if err == nil {
		t.Fatalf("expected unsupported output error")
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

	if _, ok := finding.ServiceAccounts["api"]; !ok {
		t.Fatalf("expected non-default service account to be reported")
	}
	if _, ok := finding.TokenSources["effective automountServiceAccountToken=true"]; !ok {
		t.Fatalf("expected automount token source to be reported")
	}
}

func TestDetectPodTokenUseSkipsNonDefaultServiceAccountWhenAutomountDisabled(t *testing.T) {
	automount := false
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "prod"},
		Spec: corev1.PodSpec{
			ServiceAccountName:           "api",
			AutomountServiceAccountToken: &automount,
		},
	}

	finding := detectPodTokenUse(pod, nil, nil)

	if len(finding.ServiceAccounts) != 0 {
		t.Fatalf("expected no finding, got %v", sortedKeys(finding.ServiceAccounts))
	}
}

func TestDetectPodTokenUseReportsProjectedNonDefaultServiceAccountToken(t *testing.T) {
	automount := false
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "prod"},
		Spec: corev1.PodSpec{
			ServiceAccountName:           "api",
			AutomountServiceAccountToken: &automount,
			Volumes: []corev1.Volume{
				{
					Name: "api-token",
					VolumeSource: corev1.VolumeSource{
						Projected: &corev1.ProjectedVolumeSource{
							Sources: []corev1.VolumeProjection{
								{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{}},
							},
						},
					},
				},
			},
		},
	}

	finding := detectPodTokenUse(pod, nil, nil)

	if _, ok := finding.ServiceAccounts["api"]; !ok {
		t.Fatalf("expected projected non-default service account token to be reported")
	}
	if _, ok := finding.TokenSources["projected serviceAccountToken volume"]; !ok {
		t.Fatalf("expected projected token source to be reported")
	}
}

func TestDetectPodTokenUseReportsLegacyNonDefaultTokenSecret(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "prod"},
		Spec: corev1.PodSpec{
			ServiceAccountName: defaultServiceAccount,
			Volumes: []corev1.Volume{
				{
					Name: "token",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{SecretName: "builder-token"},
					},
				},
			},
		},
	}
	secrets := map[types.NamespacedName]string{
		namespacedName("prod", "builder-token"): "builder",
	}

	finding := detectPodTokenUse(pod, nil, secrets)

	if _, ok := finding.ServiceAccounts["builder"]; !ok {
		t.Fatalf("expected legacy non-default service account token Secret to be reported")
	}
}

func TestResolvePodOwnerUsesControllerOwnerReference(t *testing.T) {
	controller := true
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-775d7f5b7d-xd9m4",
			Namespace: "prod",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "api-775d7f5b7d", Controller: &controller},
			},
		},
	}

	owner := resolvePodOwner(pod)

	if owner.Kind != "ReplicaSet" || owner.Name != "api-775d7f5b7d" || owner.Namespace != "prod" {
		t.Fatalf("expected prod ReplicaSet/api-775d7f5b7d, got %#v", owner)
	}
}

func TestResolvePodOwnerUsesFirstOwnerReferenceWhenControllerMissing(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "custom-owned-pod",
			Namespace: "prod",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "Widget", Name: "custom-owner"},
			},
		},
	}

	owner := resolvePodOwner(pod)

	if owner.Kind != "Widget" || owner.Name != "custom-owner" || owner.Namespace != "prod" {
		t.Fatalf("expected prod Widget/custom-owner, got %#v", owner)
	}
}

func TestResolvePodOwnerFallsBackToPod(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "standalone", Namespace: "prod"},
	}

	owner := resolvePodOwner(pod)

	if owner.Kind != "Pod" || owner.Name != "standalone" || owner.Namespace != "prod" {
		t.Fatalf("expected prod Pod/standalone, got %#v", owner)
	}
}
