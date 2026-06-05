package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
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
				Namespace:            "prod",
				OwnerKind:            "ReplicaSet",
				OwnerName:            "api-775d7f5b7d",
				ServiceAccounts:      []string{"api"},
				TokenServiceAccounts: []string{"builder"},
				TokenSecrets: []tokenSecretFinding{
					{Name: "builder-token"},
				},
			},
		},
	}
	var output bytes.Buffer

	if err := writeResult(&output, result, options{output: "csv"}); err != nil {
		t.Fatalf("writeResult returned error: %v", err)
	}

	want := "namespace,ownerKind,ownerName,serviceAccounts,tokenServiceAccounts,tokenSecrets\nprod,ReplicaSet,api-775d7f5b7d,api,builder,builder-token\n"
	if output.String() != want {
		t.Fatalf("unexpected csv output:\nwant %q\ngot  %q", want, output.String())
	}
}

func TestWriteCSVOutputsHeaderOnlyWhenNoFindings(t *testing.T) {
	var output bytes.Buffer

	if err := writeResult(&output, scanResult{}, options{output: "csv"}); err != nil {
		t.Fatalf("writeResult returned error: %v", err)
	}

	want := "namespace,ownerKind,ownerName,serviceAccounts,tokenServiceAccounts,tokenSecrets\n"
	if got := output.String(); got != want {
		t.Fatalf("unexpected csv output %q", got)
	}
}

func TestWriteResultRejectsUnsupportedOutput(t *testing.T) {
	err := writeResult(&strings.Builder{}, scanResult{}, options{output: "yaml"})
	if err == nil {
		t.Fatalf("expected unsupported output error")
	}
}

func TestDetectLegacyServiceAccountTokenSecretReportsMetadata(t *testing.T) {
	secret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "builder-token",
			Namespace: "prod",
			Annotations: map[string]string{
				corev1.ServiceAccountNameKey:  "builder",
				corev1.ServiceAccountUIDKey:   "uid-123",
				legacyTokenLastUsedAnnotation: "2026-06-04",
				legacyTokenInvalidAnnotation:  "2026-07-04",
			},
		},
		Type: corev1.SecretTypeServiceAccountToken,
		Data: map[string][]byte{
			corev1.ServiceAccountTokenKey: []byte("token"),
		},
	}

	tokenSecret, ok := detectLegacyServiceAccountTokenSecret(secret)

	if !ok {
		t.Fatalf("expected Secret to be detected")
	}
	if tokenSecret.ServiceAccount != "builder" || tokenSecret.ServiceAccountUID != "uid-123" {
		t.Fatalf("unexpected service account metadata: %#v", tokenSecret)
	}
	if !tokenSecret.HasToken {
		t.Fatalf("expected HasToken to be true")
	}
	if tokenSecret.LastUsed != "2026-06-04" || tokenSecret.InvalidSince != "2026-07-04" {
		t.Fatalf("unexpected legacy token timestamps: %#v", tokenSecret)
	}
}

func TestDetectLegacyServiceAccountTokenSecretSkipsOpaqueSecret(t *testing.T) {
	_, ok := detectLegacyServiceAccountTokenSecret(corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-secret", Namespace: "prod"},
		Type:       corev1.SecretTypeOpaque,
	})

	if ok {
		t.Fatalf("expected Opaque Secret to be skipped")
	}
}

func TestCollectPodSecretReferencesReportsVolumeProjectedEnvAndEnvFrom(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "prod"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "token",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{SecretName: "builder-token"},
					},
				},
				{
					Name: "projected",
					VolumeSource: corev1.VolumeSource{
						Projected: &corev1.ProjectedVolumeSource{
							Sources: []corev1.VolumeProjection{
								{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "runner-token"}}},
							},
						},
					},
				},
			},
			InitContainers: []corev1.Container{
				{
					Name: "init",
					Env: []corev1.EnvVar{
						{
							Name: "TOKEN",
							ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "init-token"},
									Key:                  corev1.ServiceAccountTokenKey,
								},
							},
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name: "api",
					EnvFrom: []corev1.EnvFromSource{
						{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "envfrom-token"}}},
					},
				},
			},
		},
	}

	refs := collectPodSecretReferences(pod)

	for _, name := range []string{"builder-token", "runner-token", "init-token", "envfrom-token"} {
		if _, ok := refs[namespacedName("prod", name)]; !ok {
			t.Fatalf("expected Secret reference %q in %#v", name, refs)
		}
	}
	if len(refs) != 4 {
		t.Fatalf("expected four Secret references, got %#v", refs)
	}
}

func TestScanClusterGroupsLegacyTokenSecretsByPodOwnerReference(t *testing.T) {
	controller := true
	client := fake.NewSimpleClientset(
		legacySATokenSecret("builder-token", "prod", "builder"),
		podWithSecretVolume("api-775d7f5b7d-a", "prod", "api-775d7f5b7d", "api", "builder-token", controller),
		podWithSecretVolume("api-775d7f5b7d-b", "prod", "api-775d7f5b7d", "api", "builder-token", controller),
	)

	result, err := scanCluster(context.Background(), client, options{namespace: "prod", maxSamples: 5})
	if err != nil {
		t.Fatalf("scanCluster returned error: %v", err)
	}

	if len(result.Items) != 1 {
		t.Fatalf("expected one application, got %#v", result.Items)
	}
	item := result.Items[0]
	if item.OwnerKind != "ReplicaSet" || item.OwnerName != "api-775d7f5b7d" {
		t.Fatalf("expected ReplicaSet/api-775d7f5b7d, got %#v", item)
	}
	if item.PodCount != 2 {
		t.Fatalf("expected two pods, got %d", item.PodCount)
	}
	if got := strings.Join(item.ServiceAccounts, ","); got != "api" {
		t.Fatalf("expected pod service account api, got %q", got)
	}
	if got := strings.Join(item.TokenServiceAccounts, ","); got != "builder" {
		t.Fatalf("expected token service account builder, got %q", got)
	}
	if len(item.TokenSecrets) != 1 || item.TokenSecrets[0].Name != "builder-token" {
		t.Fatalf("expected one token Secret, got %#v", item.TokenSecrets)
	}
}

func TestScanClusterIgnoresNonServiceAccountTokenSecrets(t *testing.T) {
	controller := true
	client := fake.NewSimpleClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "app-secret", Namespace: "prod"},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"token": []byte("not-a-sa-token")},
		},
		podWithSecretVolume("api-775d7f5b7d-a", "prod", "api-775d7f5b7d", "api", "app-secret", controller),
	)

	result, err := scanCluster(context.Background(), client, options{namespace: "prod", maxSamples: 5})
	if err != nil {
		t.Fatalf("scanCluster returned error: %v", err)
	}

	if len(result.Items) != 0 {
		t.Fatalf("expected no applications, got %#v", result.Items)
	}
}

func TestScanClusterReportsUnreferencedTokenSecrets(t *testing.T) {
	client := fake.NewSimpleClientset(legacySATokenSecret("unused-token", "prod", "unused"))

	result, err := scanCluster(context.Background(), client, options{namespace: "prod", maxSamples: 5})
	if err != nil {
		t.Fatalf("scanCluster returned error: %v", err)
	}

	if len(result.Items) != 0 {
		t.Fatalf("expected no application findings, got %#v", result.Items)
	}
	if len(result.UnreferencedTokenSecrets) != 1 || result.UnreferencedTokenSecrets[0].Name != "unused-token" {
		t.Fatalf("expected unreferenced token Secret, got %#v", result.UnreferencedTokenSecrets)
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

func legacySATokenSecret(name, namespace, serviceAccount string) runtime.Object {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Annotations: map[string]string{
				corev1.ServiceAccountNameKey: serviceAccount,
				corev1.ServiceAccountUIDKey:  "uid-" + serviceAccount,
			},
		},
		Type: corev1.SecretTypeServiceAccountToken,
		Data: map[string][]byte{
			corev1.ServiceAccountTokenKey: []byte("token"),
		},
	}
}

func podWithSecretVolume(name, namespace, ownerName, serviceAccount, secretName string, controller bool) runtime.Object {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: ownerName, Controller: &controller},
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: serviceAccount,
			Volumes: []corev1.Volume{
				{
					Name: "token",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{SecretName: secretName},
					},
				},
			},
		},
	}
}
