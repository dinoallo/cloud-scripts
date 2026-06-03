package main

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

const sampleKubeconfig = `
apiVersion: v1
kind: Config
clusters:
- name: prod
  cluster:
    server: https://kubernetes.example.com
contexts:
- name: prod
  context:
    cluster: prod
    user: prod-user
current-context: prod
users:
- name: prod-user
  user:
    token: redacted
`

func TestRunPrintsHelpWithoutError(t *testing.T) {
	var stdout strings.Builder
	var stderr strings.Builder

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

func TestDetectKubeconfigsInConfigMapReportsPlainAndBase64Values(t *testing.T) {
	configMap := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kubeconfigs", Namespace: "prod"},
		Data: map[string]string{
			"admin.conf": sampleKubeconfig,
			"encoded":    base64.StdEncoding.EncodeToString([]byte(sampleKubeconfig)),
			"notes":      "not a kubeconfig",
		},
	}

	finding := detectKubeconfigsInConfigMap(configMap)

	if len(finding.KubeconfigKeys) != 2 {
		t.Fatalf("expected two kubeconfig keys, got %#v", finding.KubeconfigKeys)
	}
	assertKubeconfigKey(t, finding.KubeconfigKeys, "admin.conf", "plain")
	assertKubeconfigKey(t, finding.KubeconfigKeys, "encoded", "base64-decoded")
}

func TestDetectKubeconfigsInSecretReportsDecodedData(t *testing.T) {
	secret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-access", Namespace: "prod"},
		Data: map[string][]byte{
			"kubeconfig": []byte(sampleKubeconfig),
		},
	}

	finding := detectKubeconfigsInSecret(secret)

	if len(finding.KubeconfigKeys) != 1 {
		t.Fatalf("expected one kubeconfig key, got %#v", finding.KubeconfigKeys)
	}
	assertKubeconfigKey(t, finding.KubeconfigKeys, "kubeconfig", "decoded")
}

func TestCollectPodConfigReferencesReportsVolumeEnvAndEnvFrom(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "prod"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "configs",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: "kubeconfigs"},
						},
					},
				},
				{
					Name: "projected",
					VolumeSource: corev1.VolumeSource{
						Projected: &corev1.ProjectedVolumeSource{
							Sources: []corev1.VolumeProjection{
								{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "cluster-access"}}},
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
							Name: "BOOTSTRAP_KUBECONFIG",
							ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "cluster-access"},
									Key:                  "kubeconfig",
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
						{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "kubeconfigs"}}},
					},
				},
			},
		},
	}

	refs := collectPodConfigReferences(pod)

	if len(refs) != 2 {
		t.Fatalf("expected two referenced resources, got %#v", refs)
	}
	if _, ok := refs[configResourceKey{Namespace: "prod", Kind: "ConfigMap", Name: "kubeconfigs"}]; !ok {
		t.Fatalf("expected ConfigMap reference")
	}
	if _, ok := refs[configResourceKey{Namespace: "prod", Kind: "Secret", Name: "cluster-access"}]; !ok {
		t.Fatalf("expected Secret reference")
	}
}

func TestScanClusterGroupsDuplicatePodsByDeploymentOwner(t *testing.T) {
	controller := true
	objects := []runtime.Object{
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "kubeconfigs", Namespace: "prod"},
			Data: map[string]string{
				"admin.conf": sampleKubeconfig,
			},
		},
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "api-775d7f5b7d",
				Namespace: "prod",
				OwnerReferences: []metav1.OwnerReference{
					{Kind: "Deployment", Name: "api", Controller: &controller},
				},
			},
		},
		podWithConfigMapVolume("api-775d7f5b7d-a", "prod", "api-775d7f5b7d", controller),
		podWithConfigMapVolume("api-775d7f5b7d-b", "prod", "api-775d7f5b7d", controller),
	}
	client := fake.NewSimpleClientset(objects...)

	result, err := scanCluster(context.Background(), client, options{namespace: "prod", maxSamples: 5})
	if err != nil {
		t.Fatalf("scanCluster returned error: %v", err)
	}

	if len(result.Items) != 1 {
		t.Fatalf("expected one application, got %#v", result.Items)
	}
	item := result.Items[0]
	if item.OwnerKind != "Deployment" || item.OwnerName != "api" {
		t.Fatalf("expected Deployment/api, got %#v", item)
	}
	if item.PodCount != 2 {
		t.Fatalf("expected two pods, got %d", item.PodCount)
	}
	if len(item.ConfigResources) != 1 {
		t.Fatalf("expected one config resource, got %#v", item.ConfigResources)
	}
}

func TestScanClusterReportsUnreferencedResources(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-access", Namespace: "prod"},
		Data: map[string][]byte{
			"kubeconfig": []byte(sampleKubeconfig),
		},
	})

	result, err := scanCluster(context.Background(), client, options{namespace: "prod", maxSamples: 5})
	if err != nil {
		t.Fatalf("scanCluster returned error: %v", err)
	}

	if len(result.Items) != 0 {
		t.Fatalf("expected no application findings, got %#v", result.Items)
	}
	if len(result.UnreferencedResources) != 1 {
		t.Fatalf("expected one unreferenced resource, got %#v", result.UnreferencedResources)
	}
}

func TestScanClusterCanSkipSecretInspection(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-access", Namespace: "prod"},
		Data: map[string][]byte{
			"kubeconfig": []byte(sampleKubeconfig),
		},
	})

	result, err := scanCluster(context.Background(), client, options{namespace: "prod", maxSamples: 5, skipSecretInspect: true})
	if err != nil {
		t.Fatalf("scanCluster returned error: %v", err)
	}

	if len(result.Items) != 0 || len(result.UnreferencedResources) != 0 {
		t.Fatalf("expected no findings when secret inspection is skipped, got %#v", result)
	}
}

func assertKubeconfigKey(t *testing.T, keys []kubeconfigKeyFinding, key, encoding string) {
	t.Helper()
	for _, finding := range keys {
		if finding.Key == key && finding.Encoding == encoding {
			return
		}
	}
	t.Fatalf("expected kubeconfig key %s:%s in %#v", key, encoding, keys)
}

func podWithConfigMapVolume(name, namespace, replicaSet string, controller bool) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: replicaSet, Controller: &controller},
			},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "kubeconfig",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: "kubeconfigs"},
						},
					},
				},
			},
		},
	}
}
