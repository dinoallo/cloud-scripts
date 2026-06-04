package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"

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

func TestScanClusterGroupsDuplicatePodsByPodOwnerReference(t *testing.T) {
	controller := true
	objects := []runtime.Object{
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "kubeconfigs", Namespace: "prod"},
			Data: map[string]string{
				"admin.conf": sampleKubeconfig,
			},
		},
		podWithConfigMapVolume("api-775d7f5b7d-a", "prod", "api-775d7f5b7d", "api", controller),
		podWithConfigMapVolume("api-775d7f5b7d-b", "prod", "api-775d7f5b7d", "builder", controller),
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
	if item.OwnerKind != "ReplicaSet" || item.OwnerName != "api-775d7f5b7d" {
		t.Fatalf("expected ReplicaSet/api-775d7f5b7d, got %#v", item)
	}
	if item.PodCount != 2 {
		t.Fatalf("expected two pods, got %d", item.PodCount)
	}
	if len(item.ConfigResources) != 1 {
		t.Fatalf("expected one config resource, got %#v", item.ConfigResources)
	}
	if got := strings.Join(item.ServiceAccounts, ","); got != "api,builder" {
		t.Fatalf("expected service accounts api,builder, got %q", got)
	}
}

func TestScanClusterReportsDefaultServiceAccount(t *testing.T) {
	controller := true
	client := fake.NewSimpleClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "kubeconfigs", Namespace: "prod"},
			Data: map[string]string{
				"admin.conf": sampleKubeconfig,
			},
		},
		podWithConfigMapVolume("api-775d7f5b7d-a", "prod", "api-775d7f5b7d", "", controller),
	)

	result, err := scanCluster(context.Background(), client, options{namespace: "prod", maxSamples: 5})
	if err != nil {
		t.Fatalf("scanCluster returned error: %v", err)
	}

	if len(result.Items) != 1 {
		t.Fatalf("expected one application, got %#v", result.Items)
	}
	if got := strings.Join(result.Items[0].ServiceAccounts, ","); got != defaultServiceAccount {
		t.Fatalf("expected default service account, got %q", got)
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

func podWithConfigMapVolume(name, namespace, replicaSet, serviceAccount string, controller bool) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: replicaSet, Controller: &controller},
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: serviceAccount,
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
