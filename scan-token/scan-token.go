package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const defaultNamespace = "default"

type options struct {
	tokenArg      string
	tokenFile     string
	namespace     string
	allNamespaces bool
	labelSelector string
	fieldSelector string
	chunk         int64
	workers       int
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, err)
		return 2
	}

	token, err := loadToken(opts)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if token == "" {
		fmt.Fprintln(stderr, "token must not be empty")
		return 2
	}
	tokenB64 := base64.StdEncoding.EncodeToString([]byte(token))
	fmt.Fprintf(stdout, "Scanning for token: %s\n", mask(token))

	client, kubeNamespace, err := buildClient()
	if err != nil {
		fmt.Fprintf(stderr, "failed to build kubernetes client: %v\n", err)
		return 2
	}

	listNamespace := resolveNamespace(opts, kubeNamespace)
	found, hadError := scanResources(ctx, client, listNamespace, opts, token, tokenB64, stdout, stderr)
	if hadError {
		return 2
	}
	if found {
		fmt.Fprintln(stderr, "One or more findings reported above.")
		return 1
	}

	fmt.Fprintln(stdout, "No occurrences of the token found in pods, configmaps, or secrets.")
	return 0
}

func parseOptions(args []string, output io.Writer) (options, error) {
	opts := options{
		chunk:   500,
		workers: runtime.NumCPU() * 4,
	}
	fs := flag.NewFlagSet("scan-token", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&opts.tokenArg, "t", "", "token string to search for")
	fs.StringVar(&opts.tokenFile, "f", "", "file containing the token (first line)")
	fs.StringVar(&opts.namespace, "n", "", "namespace to scan (default: current context namespace)")
	fs.BoolVar(&opts.allNamespaces, "A", false, "scan all namespaces (overrides -n)")
	fs.StringVar(&opts.labelSelector, "l", "", "label selector for listed resources")
	fs.StringVar(&opts.fieldSelector, "F", "", "field selector for listed resources")
	fs.Int64Var(&opts.chunk, "c", opts.chunk, "page size for list requests")
	fs.IntVar(&opts.workers, "w", opts.workers, "number of concurrent workers")

	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	tokenProvided := false
	fileProvided := false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "t":
			tokenProvided = true
		case "f":
			fileProvided = true
		}
	})
	if fs.NArg() > 0 {
		return opts, fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}
	if !tokenProvided && !fileProvided {
		fs.Usage()
		return opts, errors.New("either -t or -f must be provided")
	}
	if opts.chunk < 0 {
		return opts, errors.New("-c must be zero or greater")
	}
	if opts.workers < 1 {
		return opts, errors.New("-w must be greater than zero")
	}
	return opts, nil
}

func loadToken(opts options) (string, error) {
	if opts.tokenFile == "" {
		return opts.tokenArg, nil
	}

	file, err := os.Open(opts.tokenFile)
	if err != nil {
		return "", fmt.Errorf("cannot read token file: %w", err)
	}
	defer file.Close()

	line, err := bufio.NewReader(file).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("cannot read token file: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func buildClient() (*kubernetes.Clientset, string, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})
	config, err := clientConfig.ClientConfig()
	if err == nil {
		namespace, _, nsErr := clientConfig.Namespace()
		if nsErr != nil || namespace == "" {
			namespace = defaultNamespace
		}
		client, clientErr := kubernetes.NewForConfig(config)
		return client, namespace, clientErr
	}

	config, err = rest.InClusterConfig()
	if err != nil {
		return nil, "", err
	}
	client, clientErr := kubernetes.NewForConfig(config)
	return client, inClusterNamespace(), clientErr
}

func inClusterNamespace() string {
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return defaultNamespace
	}
	if namespace := strings.TrimSpace(string(data)); namespace != "" {
		return namespace
	}
	return defaultNamespace
}

func resolveNamespace(opts options, kubeNamespace string) string {
	if opts.allNamespaces {
		return metav1.NamespaceAll
	}
	if opts.namespace != "" {
		return opts.namespace
	}
	if kubeNamespace != "" {
		return kubeNamespace
	}
	return defaultNamespace
}

func mask(s string) string {
	if len(s) <= 8 {
		return "********"
	}
	return s[:4] + "..." + s[len(s)-4:]
}

func scanResources(ctx context.Context, client kubernetes.Interface, namespace string, opts options, token, tokenB64 string, stdout, stderr io.Writer) (bool, bool) {
	var found int32
	var hadError int32
	var wg sync.WaitGroup
	var outputMu sync.Mutex
	sem := make(chan struct{}, opts.workers)

	report := func(message string) {
		atomic.StoreInt32(&found, 1)
		outputMu.Lock()
		defer outputMu.Unlock()
		fmt.Fprintf(stdout, "[FOUND] %s\n", message)
	}
	recordError := func(format string, args ...interface{}) {
		atomic.StoreInt32(&hadError, 1)
		outputMu.Lock()
		defer outputMu.Unlock()
		fmt.Fprintf(stderr, format+"\n", args...)
	}
	runItem := func(fn func()) {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fn()
		}()
	}
	listOptions := func(continueToken string) metav1.ListOptions {
		return metav1.ListOptions{
			Limit:         opts.chunk,
			Continue:      continueToken,
			LabelSelector: opts.labelSelector,
			FieldSelector: opts.fieldSelector,
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		var continueToken string
		for {
			cmList, err := client.CoreV1().ConfigMaps(namespace).List(ctx, listOptions(continueToken))
			if err != nil {
				recordError("error listing configmaps: %v", err)
				return
			}
			for _, cm := range cmList.Items {
				cm := cm
				runItem(func() {
					for _, finding := range configMapFindings(cm, token) {
						report(finding)
					}
				})
			}
			if cmList.Continue == "" {
				return
			}
			continueToken = cmList.Continue
			time.Sleep(10 * time.Millisecond)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		var continueToken string
		for {
			secretList, err := client.CoreV1().Secrets(namespace).List(ctx, listOptions(continueToken))
			if err != nil {
				recordError("error listing secrets: %v", err)
				return
			}
			for _, secret := range secretList.Items {
				secret := secret
				runItem(func() {
					for _, finding := range secretFindings(secret, token, tokenB64) {
						report(finding)
					}
				})
			}
			if secretList.Continue == "" {
				return
			}
			continueToken = secretList.Continue
			time.Sleep(10 * time.Millisecond)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		var continueToken string
		for {
			podList, err := client.CoreV1().Pods(namespace).List(ctx, listOptions(continueToken))
			if err != nil {
				recordError("error listing pods: %v", err)
				return
			}
			for _, pod := range podList.Items {
				pod := pod
				runItem(func() {
					for _, finding := range podFindings(pod, token) {
						report(finding)
					}
				})
			}
			if podList.Continue == "" {
				return
			}
			continueToken = podList.Continue
			time.Sleep(10 * time.Millisecond)
		}
	}()

	wg.Wait()
	return atomic.LoadInt32(&found) == 1, atomic.LoadInt32(&hadError) == 1
}

func configMapFindings(cm corev1.ConfigMap, token string) []string {
	var findings []string
	for key, value := range cm.Data {
		if strings.Contains(value, token) {
			findings = append(findings, fmt.Sprintf("ConfigMap %s/%s key=%s contains token", cm.Namespace, cm.Name, key))
		}
	}
	needle := []byte(token)
	for key, value := range cm.BinaryData {
		if bytes.Contains(value, needle) {
			findings = append(findings, fmt.Sprintf("ConfigMap %s/%s binaryKey=%s contains token", cm.Namespace, cm.Name, key))
		}
	}
	return findings
}

func secretFindings(secret corev1.Secret, token, tokenB64 string) []string {
	var findings []string
	needle := []byte(token)
	for key, value := range secret.Data {
		if len(value) == 0 {
			continue
		}
		if base64.StdEncoding.EncodeToString(value) == tokenB64 {
			findings = append(findings, fmt.Sprintf("Secret %s/%s key=%s stores the token (base64 match)", secret.Namespace, secret.Name, key))
			continue
		}
		if bytes.Contains(value, needle) {
			findings = append(findings, fmt.Sprintf("Secret %s/%s key=%s contains token (decoded)", secret.Namespace, secret.Name, key))
		}
	}
	return findings
}

func podFindings(pod corev1.Pod, token string) []string {
	var findings []string
	containers := append([]corev1.Container{}, pod.Spec.Containers...)
	containers = append(containers, pod.Spec.InitContainers...)
	for _, container := range containers {
		for _, env := range container.Env {
			if env.Value != "" && strings.Contains(env.Value, token) {
				findings = append(findings, fmt.Sprintf("Pod %s/%s container=%s env var=%s contains token", pod.Namespace, pod.Name, container.Name, env.Name))
			}
		}
	}
	return findings
}
