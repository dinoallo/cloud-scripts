package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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
				Confidence:      confidenceLikely,
				ServiceAccounts: []string{"api"},
				Reasons:         []string{"serviceAccount has RBAC binding"},
				SamplePods:      []string{"api-0"},
			},
		},
	}
	var output bytes.Buffer

	if err := writeResult(&output, result, options{output: "csv"}); err != nil {
		t.Fatalf("writeResult returned error: %v", err)
	}

	want := "namespace,ownerKind,ownerName,confidence,serviceAccounts,reasons,pods\n" +
		"prod,ReplicaSet,api-775d7f5b7d,likely,api,serviceAccount has RBAC binding,api-0\n"
	if output.String() != want {
		t.Fatalf("unexpected csv output:\nwant %q\ngot  %q", want, output.String())
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

func TestLoadServiceAccountRisksIncludesRoleAndClusterRoleBindings(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "prod"}},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "api-reader", Namespace: "prod"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "api"}},
		},
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "runner-admin"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "runner", Namespace: "prod"}},
		},
	)

	risks, err := loadServiceAccountRisks(context.Background(), client, "prod")
	if err != nil {
		t.Fatalf("loadServiceAccountRisks returned error: %v", err)
	}

	apiRisk := risks[serviceAccountKey{Namespace: "prod", Name: "api"}]
	if !apiRisk.HasRBAC {
		t.Fatalf("expected api service account to have RBAC")
	}
	if _, ok := apiRisk.BindingRefs["RoleBinding/prod/api-reader"]; !ok {
		t.Fatalf("expected role binding ref, got %#v", apiRisk.BindingRefs)
	}
	runnerRisk := risks[serviceAccountKey{Namespace: "prod", Name: "runner"}]
	if !runnerRisk.HasRBAC {
		t.Fatalf("expected runner service account to have RBAC")
	}
	if _, ok := runnerRisk.BindingRefs["ClusterRoleBinding/runner-admin"]; !ok {
		t.Fatalf("expected cluster role binding ref, got %#v", runnerRisk.BindingRefs)
	}
}

func TestDetectPodReportsLikelyWhenServiceAccountHasRBACAndAutomount(t *testing.T) {
	pod := podWithServiceAccount("api-0", "prod", "api")
	risks := map[serviceAccountKey]serviceAccountRisk{
		{Namespace: "prod", Name: "api"}: {
			HasRBAC:     true,
			BindingRefs: map[string]struct{}{"RoleBinding/prod/api-reader": {}},
		},
	}

	finding := detectPodAPIServerAccessCandidate(*pod, risks, nil)

	if finding.Confidence != confidenceLikely {
		t.Fatalf("expected likely confidence, got %#v", finding)
	}
	assertStringInSlice(t, finding.Reasons, "serviceAccount has RBAC binding")
	assertStringInSlice(t, finding.Reasons, "effective automountServiceAccountToken=true")
	assertStringInSlice(t, finding.Reasons, "bound by RoleBinding/prod/api-reader")
}

func TestDetectPodReportsPossibleForAutomountedDefaultTokenWithoutRBAC(t *testing.T) {
	pod := podWithServiceAccount("api-0", "prod", "default")

	finding := detectPodAPIServerAccessCandidate(*pod, nil, nil)

	if finding.Confidence != confidencePossible {
		t.Fatalf("expected possible confidence, got %#v", finding)
	}
	assertStringInSlice(t, finding.Reasons, "effective automountServiceAccountToken=true")
}

func TestDetectPodSkipsWhenAutomountDisabledAndNoOtherSignal(t *testing.T) {
	automount := false
	pod := podWithServiceAccount("api-0", "prod", "default")
	pod.Spec.AutomountServiceAccountToken = &automount

	finding := detectPodAPIServerAccessCandidate(*pod, nil, nil)

	if finding.Confidence != "" {
		t.Fatalf("expected no candidate, got %#v", finding)
	}
}

func TestDetectPodReportsLikelyForExplicitProjectedServiceAccountToken(t *testing.T) {
	automount := false
	pod := podWithServiceAccount("api-0", "prod", "api")
	pod.Spec.AutomountServiceAccountToken = &automount
	pod.Spec.Volumes = []corev1.Volume{
		{
			Name: "api-token",
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{
						{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}},
					},
				},
			},
		},
	}

	finding := detectPodAPIServerAccessCandidate(*pod, nil, nil)

	if finding.Confidence != confidenceLikely {
		t.Fatalf("expected likely confidence, got %#v", finding)
	}
	assertStringInSlice(t, finding.Reasons, "has explicit projected serviceAccountToken volume")
}

func TestDetectPodReportsLikelyForReferencedKubeconfigResource(t *testing.T) {
	automount := false
	pod := podWithServiceAccount("api-0", "prod", "default")
	pod.Spec.AutomountServiceAccountToken = &automount
	pod.Spec.Volumes = []corev1.Volume{
		{
			Name: "kubeconfig",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "kubeconfigs"},
				},
			},
		},
	}
	resources := map[configResourceKey]detectedConfigResource{
		{Namespace: "prod", Kind: "ConfigMap", Name: "kubeconfigs"}: {
			Namespace: "prod",
			Kind:      "ConfigMap",
			Name:      "kubeconfigs",
			KubeconfigKeys: []kubeconfigKeyFinding{
				{Key: "admin.conf", Encoding: "plain"},
			},
		},
	}

	finding := detectPodAPIServerAccessCandidate(*pod, nil, resources)

	if finding.Confidence != confidenceLikely {
		t.Fatalf("expected likely confidence, got %#v", finding)
	}
	if len(finding.ConfigResources) != 1 {
		t.Fatalf("expected one config resource, got %#v", finding.ConfigResources)
	}
	assertStringInSlice(t, finding.Reasons, "references kubeconfig ConfigMap/kubeconfigs")
}

func TestScanClusterGroupsFindingsByOwnerReference(t *testing.T) {
	controller := true
	client := fake.NewSimpleClientset(
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "prod"}},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "api-reader", Namespace: "prod"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "api"}},
		},
		podWithOwner("api-775d7f5b7d-a", "prod", "api-775d7f5b7d", "api", controller),
		podWithOwner("api-775d7f5b7d-b", "prod", "api-775d7f5b7d", "api", controller),
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
	if item.Confidence != confidenceLikely {
		t.Fatalf("expected likely confidence, got %#v", item)
	}
	if item.PodCount != 2 {
		t.Fatalf("expected two pods, got %d", item.PodCount)
	}
}

func TestScanClusterReportsUnreferencedKubeconfigResources(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kubeconfigs", Namespace: "prod"},
		Data: map[string]string{
			"admin.conf": sampleKubeconfig,
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
		t.Fatalf("expected one unreferenced kubeconfig resource, got %#v", result.UnreferencedResources)
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

	if len(result.UnreferencedResources) != 0 {
		t.Fatalf("expected no unreferenced Secret findings when secret inspection is skipped, got %#v", result.UnreferencedResources)
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

func assertKubeconfigKey(t *testing.T, keys []kubeconfigKeyFinding, key, encoding string) {
	t.Helper()
	for _, finding := range keys {
		if finding.Key == key && finding.Encoding == encoding {
			return
		}
	}
	t.Fatalf("expected kubeconfig key %s:%s in %#v", key, encoding, keys)
}

func assertStringInSlice(t *testing.T, values []string, want string) {
	t.Helper()
	for _, value := range values {
		if value == want {
			return
		}
	}
	t.Fatalf("expected %q in %#v", want, values)
}

func podWithServiceAccount(name, namespace, serviceAccount string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PodSpec{
			ServiceAccountName: serviceAccount,
		},
	}
}

func podWithOwner(name, namespace, replicaSet, serviceAccount string, controller bool) *corev1.Pod {
	pod := podWithServiceAccount(name, namespace, serviceAccount)
	pod.OwnerReferences = []metav1.OwnerReference{
		{Kind: "ReplicaSet", Name: replicaSet, Controller: &controller},
	}
	return pod
}

var _ runtime.Object = (*corev1.Pod)(nil)
