package main

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSecretFindingsReportsExactTokenAsBase64Match(t *testing.T) {
	token := "secret-token"
	secret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-secret", Namespace: "prod"},
		Data: map[string][]byte{
			"token": []byte(token),
		},
	}

	findings := secretFindings(secret, token, base64.StdEncoding.EncodeToString([]byte(token)))

	if len(findings) != 1 {
		t.Fatalf("expected one finding, got %d: %v", len(findings), findings)
	}
	if !strings.Contains(findings[0], "base64 match") {
		t.Fatalf("expected base64 match finding, got %q", findings[0])
	}
}

func TestSecretFindingsReportsEmbeddedDecodedToken(t *testing.T) {
	token := "secret-token"
	secret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-secret", Namespace: "prod"},
		Data: map[string][]byte{
			"config": []byte("prefix secret-token suffix"),
		},
	}

	findings := secretFindings(secret, token, base64.StdEncoding.EncodeToString([]byte(token)))

	if len(findings) != 1 {
		t.Fatalf("expected one finding, got %d: %v", len(findings), findings)
	}
	if !strings.Contains(findings[0], "decoded") {
		t.Fatalf("expected decoded finding, got %q", findings[0])
	}
}

func TestConfigMapFindingsReportsTextAndBinaryData(t *testing.T) {
	cm := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "app-config", Namespace: "prod"},
		Data: map[string]string{
			"settings": "token=secret-token",
		},
		BinaryData: map[string][]byte{
			"blob": []byte("secret-token"),
		},
	}

	findings := configMapFindings(cm, "secret-token")

	if len(findings) != 2 {
		t.Fatalf("expected two findings, got %d: %v", len(findings), findings)
	}
}

func TestPodFindingsReportsContainerAndInitContainerEnv(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "prod"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "api",
					Env: []corev1.EnvVar{
						{Name: "TOKEN", Value: "secret-token"},
					},
				},
			},
			InitContainers: []corev1.Container{
				{
					Name: "init",
					Env: []corev1.EnvVar{
						{Name: "BOOTSTRAP_TOKEN", Value: "secret-token"},
					},
				},
			},
		},
	}

	findings := podFindings(pod, "secret-token")

	if len(findings) != 2 {
		t.Fatalf("expected two findings, got %d: %v", len(findings), findings)
	}
}

func TestResolveNamespaceUsesCurrentContextNamespace(t *testing.T) {
	got := resolveNamespace(options{}, "prod")
	if got != "prod" {
		t.Fatalf("expected prod, got %q", got)
	}
}

func TestResolveNamespaceAllOverridesExplicitNamespace(t *testing.T) {
	got := resolveNamespace(options{allNamespaces: true, namespace: "prod"}, "stage")
	if got != metav1.NamespaceAll {
		t.Fatalf("expected all namespaces, got %q", got)
	}
}

func TestRunRejectsEmptyToken(t *testing.T) {
	var stdout strings.Builder
	var stderr strings.Builder

	code := run(context.Background(), []string{"-t", ""}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "token must not be empty") {
		t.Fatalf("expected empty token error, got %q", stderr.String())
	}
}
