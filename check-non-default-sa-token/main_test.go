package main

import (
	"bytes"
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
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

func TestOwnerResolverPromotesReplicaSetToDeployment(t *testing.T) {
	controller := true
	replicaSet := appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-775d7f5b7d",
			Namespace: "prod",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "Deployment", Name: "api", Controller: &controller},
			},
		},
	}
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-775d7f5b7d-xd9m4",
			Namespace: "prod",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "api-775d7f5b7d", Controller: &controller},
			},
		},
	}
	resolver := ownerResolver{
		replicaSetOwners: map[types.NamespacedName]*metav1.OwnerReference{
			namespacedName(replicaSet.Namespace, replicaSet.Name): controllerOwner(&replicaSet.ObjectMeta),
		},
		jobOwners: map[types.NamespacedName]*metav1.OwnerReference{},
	}

	owner := resolver.resolvePodOwner(pod)

	if owner.Kind != "Deployment" || owner.Name != "api" || owner.Namespace != "prod" {
		t.Fatalf("expected prod Deployment/api, got %#v", owner)
	}
}

func TestOwnerResolverPromotesJobToCronJob(t *testing.T) {
	controller := true
	job := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-28678000",
			Namespace: "prod",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "CronJob", Name: "backup", Controller: &controller},
			},
		},
	}
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-28678000-k8j7p",
			Namespace: "prod",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "Job", Name: "backup-28678000", Controller: &controller},
			},
		},
	}
	resolver := ownerResolver{
		replicaSetOwners: map[types.NamespacedName]*metav1.OwnerReference{},
		jobOwners: map[types.NamespacedName]*metav1.OwnerReference{
			namespacedName(job.Namespace, job.Name): controllerOwner(&job.ObjectMeta),
		},
	}

	owner := resolver.resolvePodOwner(pod)

	if owner.Kind != "CronJob" || owner.Name != "backup" || owner.Namespace != "prod" {
		t.Fatalf("expected prod CronJob/backup, got %#v", owner)
	}
}
