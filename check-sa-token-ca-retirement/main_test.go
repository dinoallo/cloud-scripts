package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type fakePodFileReader map[string][]byte

func (reader fakePodFileReader) ReadFile(_ context.Context, pod corev1.Pod, container, filePath string) ([]byte, error) {
	key := pod.Namespace + "/" + pod.Name + "/" + container + ":" + filePath
	data, ok := reader[key]
	if !ok {
		return nil, errFakeReadFailure(key)
	}
	return data, nil
}

type errFakeReadFailure string

func (err errFakeReadFailure) Error() string {
	return "missing fake file: " + string(err)
}

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

func TestParseOptionsParsesCutoverAndNewCAFile(t *testing.T) {
	certPEM := mustTestCertificatePEM(t, "new-ca")
	caFile := t.TempDir() + "/new-ca.pem"
	t.Setenv("HOME", t.TempDir())
	if err := osWriteFile(caFile, certPEM); err != nil {
		t.Fatalf("write temp CA file: %v", err)
	}

	opts, err := parseOptions([]string{
		"--sa-key-cutover", "2026-06-08T08:00:00Z",
		"--new-ca-file", caFile,
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseOptions returned error: %v", err)
	}
	if opts.saKeyCutover == nil || formatTime(*opts.saKeyCutover) != "2026-06-08T08:00:00Z" {
		t.Fatalf("unexpected cutover: %#v", opts.saKeyCutover)
	}
	if len(opts.requiredCAFingerprints) != 1 {
		t.Fatalf("expected one required CA fingerprint, got %#v", opts.requiredCAFingerprints)
	}
}

func TestWriteCSVOutputsDefaultColumns(t *testing.T) {
	result := scanResult{
		Items: []applicationFinding{
			{
				Namespace:                  "prod",
				OwnerKind:                  "ReplicaSet",
				OwnerName:                  "api-775d7f5b7d",
				ServiceAccounts:            []string{"api"},
				IssueTypes:                 []string{"legacy_secret_token_in_use"},
				MaxBlockingTokenExpiration: "2026-06-08T09:00:00Z",
				LegacyTokenSecrets:         []legacyTokenSecretFinding{{Name: "builder-token"}},
				SamplePods:                 []string{"api-0"},
			},
		},
	}
	var output bytes.Buffer

	if err := writeResult(&output, result, options{output: "csv"}); err != nil {
		t.Fatalf("writeResult returned error: %v", err)
	}

	want := "namespace,ownerKind,ownerName,serviceAccounts,issueTypes,maxBlockingTokenExpiration,legacyTokenSecrets,pods\n" +
		"prod,ReplicaSet,api-775d7f5b7d,api,legacy_secret_token_in_use,2026-06-08T09:00:00Z,builder-token,api-0\n"
	if output.String() != want {
		t.Fatalf("unexpected csv output:\nwant %q\ngot  %q", want, output.String())
	}
}

func TestWriteCSVOutputsHeaderOnlyWhenNoFindings(t *testing.T) {
	var output bytes.Buffer

	if err := writeResult(&output, scanResult{}, options{output: "csv"}); err != nil {
		t.Fatalf("writeResult returned error: %v", err)
	}

	want := "namespace,ownerKind,ownerName,serviceAccounts,issueTypes,maxBlockingTokenExpiration,legacyTokenSecrets,pods\n"
	if got := output.String(); got != want {
		t.Fatalf("unexpected csv output %q", got)
	}
}

func TestDecodeJWTTimestamps(t *testing.T) {
	token := testJWT(t, 1780905600, 1780909200)

	timestamps, err := decodeJWTTimestamps(token)
	if err != nil {
		t.Fatalf("decodeJWTTimestamps returned error: %v", err)
	}
	if timestamps.IssuedAt == nil || formatTime(*timestamps.IssuedAt) != "2026-06-08T08:00:00Z" {
		t.Fatalf("unexpected iat: %#v", timestamps.IssuedAt)
	}
	if timestamps.ExpiresAt == nil || formatTime(*timestamps.ExpiresAt) != "2026-06-08T09:00:00Z" {
		t.Fatalf("unexpected exp: %#v", timestamps.ExpiresAt)
	}
}

func TestCertificateFingerprintsRequireAllCertificates(t *testing.T) {
	first := mustTestCertificatePEM(t, "first")
	second := mustTestCertificatePEM(t, "second")

	required, err := certificateFingerprints(append(first, second...))
	if err != nil {
		t.Fatalf("certificateFingerprints returned error: %v", err)
	}
	actual, err := certificateFingerprints(first)
	if err != nil {
		t.Fatalf("certificateFingerprints returned error: %v", err)
	}

	if containsAllFingerprints(actual, required) {
		t.Fatalf("expected single-cert bundle not to satisfy two required certs")
	}
	actual, err = certificateFingerprints(append(first, second...))
	if err != nil {
		t.Fatalf("certificateFingerprints returned error: %v", err)
	}
	if !containsAllFingerprints(actual, required) {
		t.Fatalf("expected full bundle to satisfy required certs")
	}
}

func TestCollectServiceAccountTokenCandidatesIncludesAutomountAndExplicitProjection(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "prod"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "api",
					VolumeMounts: []corev1.VolumeMount{
						{Name: "custom-token", MountPath: "/tokens"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "custom-token",
					VolumeSource: corev1.VolumeSource{
						Projected: &corev1.ProjectedVolumeSource{
							Sources: []corev1.VolumeProjection{
								{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "custom.jwt"}},
							},
						},
					},
				},
			},
		},
	}

	candidates := collectServiceAccountTokenCandidates(pod, pod.Spec.Containers[0], defaultTokenPath, nil)

	if got := candidatePaths(candidates); strings.Join(got, ",") != "/tokens/custom.jwt,"+defaultTokenPath {
		t.Fatalf("unexpected token candidates: %#v", candidates)
	}
}

func TestInspectPodReportsOldProjectedToken(t *testing.T) {
	cutover := mustTime(t, "2026-06-08T08:00:00Z")
	now := mustTime(t, "2026-06-08T08:30:00Z")
	pod := runningPod("api-0", "prod", "api")
	reader := fakePodFileReader{
		"prod/api-0/api:" + defaultTokenPath: []byte(testJWT(t, 1780902000, 1780909200)),
	}

	finding := inspectPod(context.Background(), reader, pod, nil, nil, options{
		saKeyCutover: &cutover,
		tokenPath:    defaultTokenPath,
		now:          now,
	})

	if len(finding.Issues) != 1 {
		t.Fatalf("expected one issue, got %#v", finding.Issues)
	}
	issue := finding.Issues[0]
	if issue.Type != "projected_token_issued_before_cutover" {
		t.Fatalf("unexpected issue: %#v", issue)
	}
	if issue.IssuedAt != "2026-06-08T07:00:00Z" || issue.ExpiresAt != "2026-06-08T09:00:00Z" {
		t.Fatalf("unexpected timestamps: %#v", issue)
	}
}

func TestInspectPodIgnoresExpiredOldProjectedToken(t *testing.T) {
	cutover := mustTime(t, "2026-06-08T08:00:00Z")
	now := mustTime(t, "2026-06-08T09:30:00Z")
	pod := runningPod("api-0", "prod", "api")
	reader := fakePodFileReader{
		"prod/api-0/api:" + defaultTokenPath: []byte(testJWT(t, 1780902000, 1780909200)),
	}

	finding := inspectPod(context.Background(), reader, pod, nil, nil, options{
		saKeyCutover: &cutover,
		tokenPath:    defaultTokenPath,
		now:          now,
	})

	if len(finding.Issues) != 0 {
		t.Fatalf("expected no issues for expired old token, got %#v", finding.Issues)
	}
}

func TestInspectPodReportsMissingNewCA(t *testing.T) {
	oldCA := mustTestCertificatePEM(t, "old-ca")
	newCA := mustTestCertificatePEM(t, "new-ca")
	required, err := certificateFingerprints(newCA)
	if err != nil {
		t.Fatalf("certificateFingerprints returned error: %v", err)
	}
	pod := runningPod("api-0", "prod", "api")
	reader := fakePodFileReader{
		"prod/api-0/api:" + defaultCACertPath: oldCA,
	}

	finding := inspectPod(context.Background(), reader, pod, nil, nil, options{
		requiredCAFingerprints: required,
		caCertPath:             defaultCACertPath,
	})

	if len(finding.Issues) != 1 {
		t.Fatalf("expected one issue, got %#v", finding.Issues)
	}
	if finding.Issues[0].Type != "ca_crt_missing_required_ca" {
		t.Fatalf("unexpected issue: %#v", finding.Issues[0])
	}
}

func TestInspectPodAcceptsNewCA(t *testing.T) {
	newCA := mustTestCertificatePEM(t, "new-ca")
	required, err := certificateFingerprints(newCA)
	if err != nil {
		t.Fatalf("certificateFingerprints returned error: %v", err)
	}
	pod := runningPod("api-0", "prod", "api")
	reader := fakePodFileReader{
		"prod/api-0/api:" + defaultCACertPath: newCA,
	}

	finding := inspectPod(context.Background(), reader, pod, nil, nil, options{
		requiredCAFingerprints: required,
		caCertPath:             defaultCACertPath,
	})

	if len(finding.Issues) != 0 {
		t.Fatalf("expected no issues for matching CA, got %#v", finding.Issues)
	}
}

func TestScanClusterGroupsLegacyTokenSecretAndProjectedTokenByOwner(t *testing.T) {
	controller := true
	cutover := mustTime(t, "2026-06-08T08:00:00Z")
	now := mustTime(t, "2026-06-08T08:30:00Z")
	pod := podWithSecretVolume("api-775d7f5b7d-a", "prod", "api-775d7f5b7d", "api", "builder-token", controller)
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "api", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
	}

	client := fake.NewSimpleClientset(
		legacySATokenSecret("builder-token", "prod", "builder"),
		pod,
	)
	reader := fakePodFileReader{
		"prod/api-775d7f5b7d-a/api:" + defaultTokenPath: []byte(testJWT(t, 1780902000, 1780909200)),
	}

	result, err := scanCluster(context.Background(), client, reader, options{
		namespace:           "prod",
		maxSamples:          5,
		saKeyCutover:        &cutover,
		tokenPath:           defaultTokenPath,
		now:                 now,
		includePodBreakdown: true,
	})
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
	if item.MaxBlockingTokenExpiration != "2026-06-08T09:00:00Z" {
		t.Fatalf("unexpected max token expiration: %#v", item)
	}
	if got := strings.Join(item.IssueTypes, ","); got != "legacy_secret_token_in_use,projected_token_issued_before_cutover" {
		t.Fatalf("unexpected issue types %q", got)
	}
	if len(item.LegacyTokenSecrets) != 1 || item.LegacyTokenSecrets[0].Name != "builder-token" {
		t.Fatalf("expected one legacy token Secret, got %#v", item.LegacyTokenSecrets)
	}
	if len(item.Pods) != 1 || len(item.Pods[0].Issues) != 2 {
		t.Fatalf("expected pod issue breakdown, got %#v", item.Pods)
	}
}

func TestScanClusterReportsUnreferencedLegacyTokenSecrets(t *testing.T) {
	client := fake.NewSimpleClientset(legacySATokenSecret("builder-token", "prod", "builder"))

	result, err := scanCluster(context.Background(), client, nil, options{namespace: "prod", maxSamples: 5})
	if err != nil {
		t.Fatalf("scanCluster returned error: %v", err)
	}

	if len(result.Items) != 0 {
		t.Fatalf("expected no application findings, got %#v", result.Items)
	}
	if len(result.UnreferencedLegacyTokenSecrets) != 1 {
		t.Fatalf("expected one unreferenced legacy token Secret, got %#v", result.UnreferencedLegacyTokenSecrets)
	}
}

func TestScanClusterCanSkipLegacySecretInspection(t *testing.T) {
	client := fake.NewSimpleClientset(legacySATokenSecret("builder-token", "prod", "builder"))

	result, err := scanCluster(context.Background(), client, nil, options{
		namespace:                  "prod",
		maxSamples:                 5,
		skipLegacySecretInspection: true,
	})
	if err != nil {
		t.Fatalf("scanCluster returned error: %v", err)
	}

	if len(result.Items) != 0 || len(result.UnreferencedLegacyTokenSecrets) != 0 {
		t.Fatalf("expected no findings when legacy Secret inspection is skipped, got %#v", result)
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

func runningPod(name, namespace, serviceAccount string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PodSpec{
			ServiceAccountName: serviceAccount,
			Containers: []corev1.Container{
				{Name: "api"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "api", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	}
}

func podWithSecretVolume(name, namespace, replicaSet, serviceAccount, secret string, controller bool) *corev1.Pod {
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
			Containers: []corev1.Container{
				{Name: "api"},
			},
			Volumes: []corev1.Volume{
				{
					Name: "legacy-token",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{SecretName: secret},
					},
				},
			},
		},
	}
}

func legacySATokenSecret(name, namespace, serviceAccount string) *corev1.Secret {
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

func candidatePaths(candidates []podFileCandidate) []string {
	paths := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		paths = append(paths, candidate.path)
	}
	sortStrings(paths)
	return paths
}

func testJWT(t *testing.T, iat, exp int64) string {
	t.Helper()
	header := map[string]interface{}{"alg": "none", "typ": "JWT"}
	payload := map[string]interface{}{"iat": iat, "exp": exp}
	return base64.RawURLEncoding.EncodeToString(mustJSON(t, header)) + "." +
		base64.RawURLEncoding.EncodeToString(mustJSON(t, payload)) + "."
}

func mustJSON(t *testing.T, value interface{}) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	return data
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed
}

func mustTestCertificatePEM(t *testing.T, commonName string) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func osWriteFile(name string, data []byte) error {
	return os.WriteFile(name, data, 0600)
}

func sortStrings(values []string) {
	sort.Strings(values)
}
