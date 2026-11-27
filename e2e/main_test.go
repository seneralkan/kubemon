//go:build e2e

// Package e2e drives the built kubemon binary against a real, throwaway
// cluster (kind in CI). Every test works in its own namespace.
//
// Required: KUBEMON_BIN, the path of the binary under test, and a kubeconfig
// (KUBECONFIG) whose current context is a kind cluster ("kind-*"). To use some
// other throwaway cluster, name its context in KUBEMON_E2E_CONTEXT. Argo tests
// are skipped when the Rollout CRD is missing, unless KUBEMON_E2E_REQUIRE_ARGO=1.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"
)

const (
	imageOld = "nginx:1.27-alpine"
	imageNew = "nginx:1.28-alpine"
	imageBad = "nginx:kubemon-e2e-does-not-exist"
)

var (
	kubemonBin  string
	kubeContext string
	skipReason  string // set when the suite cannot run; every test skips with it
	haveArgo    bool
)

//go:embed testdata/*.yaml
var testdata embed.FS

var manifests = template.Must(template.ParseFS(testdata, "testdata/*.yaml"))

func TestMain(m *testing.M) {
	if err := setup(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// setup finds the binary and checks the cluster is one we may break. It does
// not contact any cluster unless KUBEMON_BIN is set.
func setup() error {
	kubemonBin = os.Getenv("KUBEMON_BIN")
	if kubemonBin == "" {
		skipReason = "KUBEMON_BIN not set: e2e tests need a built kubemon binary and a throwaway cluster"
		return nil
	}
	abs, err := filepath.Abs(kubemonBin)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err != nil {
		return fmt.Errorf("KUBEMON_BIN: %w", err)
	}
	kubemonBin = abs

	out, err := exec.Command("kubectl", "config", "current-context").CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl config current-context: %w: %s", err, out)
	}
	kubeContext = strings.TrimSpace(string(out))
	switch allowed := os.Getenv("KUBEMON_E2E_CONTEXT"); {
	case allowed != "" && kubeContext != allowed:
		return fmt.Errorf("current context %q is not KUBEMON_E2E_CONTEXT %q", kubeContext, allowed)
	case allowed == "" && !strings.HasPrefix(kubeContext, "kind-"):
		return fmt.Errorf("refusing to run against context %q: the tests create and delete namespaces, "+
			"so use a throwaway kind cluster, or set KUBEMON_E2E_CONTEXT to that context's name", kubeContext)
	}

	haveArgo = exec.Command("kubectl", "--context", kubeContext, "get", "crd", "rollouts.argoproj.io").Run() == nil
	if !haveArgo && os.Getenv("KUBEMON_E2E_REQUIRE_ARGO") == "1" {
		return errors.New("KUBEMON_E2E_REQUIRE_ARGO=1 but the rollouts.argoproj.io CRD is not installed")
	}
	return nil
}

// env is one test's namespace on the cluster.
type env struct {
	t  *testing.T
	ns string
}

// newEnv creates a namespace for t, deleted when t ends. On failure the
// namespace's objects and events are logged first.
func newEnv(t *testing.T) *env {
	t.Helper()
	if skipReason != "" {
		t.Skip(skipReason)
	}
	e := &env{t: t, ns: namespaceName(t.Name())}
	if out, err := kubectl("create", "namespace", e.ns); err != nil {
		t.Fatalf("create namespace: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = kubectl("delete", "namespace", e.ns, "--wait=false") })
	t.Cleanup(func() {
		if t.Failed() {
			e.dump()
		}
	})
	return e
}

var nonName = regexp.MustCompile(`[^a-z0-9]+`)

func namespaceName(test string) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	name := strings.Trim(nonName.ReplaceAllString(strings.ToLower(strings.TrimPrefix(test, "Test")), "-"), "-")
	if len(name) > 40 {
		name = strings.TrimRight(name[:40], "-")
	}
	return "e2e-" + name + "-" + hex.EncodeToString(b)
}

func (e *env) dump() {
	for _, args := range [][]string{
		{"get", "all,events", "-o", "wide"},
		{"get", "deployments,replicasets", "-o", "yaml"},
	} {
		out, _ := e.kubectlErr(args...)
		e.t.Logf("$ kubectl -n %s %s\n%s", e.ns, strings.Join(args, " "), out)
	}
	if haveArgo {
		out, _ := e.kubectlErr("get", "rollouts.argoproj.io", "-o", "yaml")
		e.t.Logf("$ kubectl -n %s get rollouts -o yaml\n%s", e.ns, out)
	}
}

// kubectl runs kubectl against the test context.
func kubectl(args ...string) (string, error) {
	return kubectlStdin("", args...)
}

func kubectlStdin(stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--context", kubeContext}, args...)...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// kubectlErr runs kubectl in the test namespace.
func (e *env) kubectlErr(args ...string) (string, error) {
	return kubectl(append([]string{"-n", e.ns}, args...)...)
}

// kubectl runs kubectl in the test namespace and fails the test on error.
func (e *env) kubectl(args ...string) string {
	e.t.Helper()
	out, err := e.kubectlErr(args...)
	if err != nil {
		e.t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// apply renders testdata/<file> with data and applies it in the namespace.
func (e *env) apply(file string, data any) {
	e.t.Helper()
	var buf bytes.Buffer
	if err := manifests.ExecuteTemplate(&buf, file, data); err != nil {
		e.t.Fatalf("render %s: %v", file, err)
	}
	if out, err := kubectlStdin(buf.String(), "-n", e.ns, "apply", "-f", "-"); err != nil {
		e.t.Fatalf("apply %s: %v\n%s\n%s", file, err, out, buf.String())
	}
}

// createDeployment creates Deployment web (app=web, 2 replicas) and waits
// until it is fully rolled out.
func (e *env) createDeployment(image string) {
	e.t.Helper()
	e.apply("deployment.yaml", struct{ Image string }{image})
	e.kubectl("rollout", "status", "deploy/web", "--timeout=3m")
}

// createRollout creates Argo Rollout web with the given canary steps and
// waits until it is Healthy.
func (e *env) createRollout(image string, steps ...string) {
	e.t.Helper()
	e.apply("rollout.yaml", struct {
		Image string
		Steps []string
	}{image, steps})
	e.waitRolloutPhase("Healthy", 3*time.Minute)
}

func (e *env) waitRolloutPhase(phase string, timeout time.Duration) {
	e.t.Helper()
	e.kubectl("wait", "rollouts.argoproj.io/web", "--for=jsonpath={.status.phase}="+phase,
		fmt.Sprintf("--timeout=%s", timeout))
}

func (e *env) setRolloutImage(image string) {
	e.t.Helper()
	e.kubectl("patch", "rollouts.argoproj.io/web", "--type", "json", "-p",
		fmt.Sprintf(`[{"op":"replace","path":"/spec/template/spec/containers/0/image","value":%q}]`, image))
}

func requireArgo(t *testing.T) {
	t.Helper()
	if skipReason == "" && !haveArgo {
		t.Skip("Argo Rollouts CRD not installed")
	}
}

// result is a finished kubemon run.
type result struct {
	stdout, stderr string
	code           int
}

// proc is a running kubemon.
type proc struct {
	e              *env
	args           []string
	stdout, stderr syncBuffer
	cancel         context.CancelFunc
	done           chan struct{}
	err            error
}

// startKubemon runs kubemon args... in the test namespace in the background.
// Its output is logged if the test fails.
func (e *env) startKubemon(args ...string) *proc {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	p := &proc{e: e, args: args, cancel: cancel, done: make(chan struct{})}
	cmd := exec.CommandContext(ctx, kubemonBin, append(args, "--context", kubeContext, "-n", e.ns)...)
	cmd.Stdout = &p.stdout
	cmd.Stderr = &p.stderr
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		e.t.Fatalf("start kubemon: %v", err)
	}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	e.t.Cleanup(func() {
		p.cancel()
		<-p.done
		if e.t.Failed() {
			e.t.Logf("$ kubemon %s\n--- stdout\n%s--- stderr\n%s", strings.Join(args, " "), p.stdout.String(), p.stderr.String())
		}
	})
	return p
}

// wait waits up to limit for kubemon to exit, killing it after that.
func (p *proc) wait(limit time.Duration) result {
	t := p.e.t
	t.Helper()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		p.cancel()
		<-p.done
		t.Errorf("kubemon %s did not exit within %s; killed", strings.Join(p.args, " "), limit)
	}
	r := result{stdout: p.stdout.String(), stderr: p.stderr.String()}
	var ee *exec.ExitError
	switch {
	case errors.As(p.err, &ee):
		r.code = ee.ExitCode()
	case p.err != nil:
		t.Fatalf("kubemon: %v", p.err)
	}
	return r
}

// kubemon runs kubemon args... to completion, killing it after limit.
func (e *env) kubemon(limit time.Duration, args ...string) result {
	e.t.Helper()
	return e.startKubemon(args...).wait(limit)
}

// syncBuffer is a bytes.Buffer safe for one writer and concurrent readers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor polls cond every second until it holds, failing the test after timeout.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(time.Second)
	}
}

func expectExit(t *testing.T, r result, want int) {
	t.Helper()
	if r.code != want {
		t.Errorf("exit code %d, want %d", r.code, want)
	}
}

// expectContains checks that text contains every substring.
func expectContains(t *testing.T, what, text string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(text, s) {
			t.Errorf("%s does not contain %q", what, s)
		}
	}
}

// expectAny checks that text contains at least one of subs.
func expectAny(t *testing.T, what, text string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if strings.Contains(text, s) {
			return
		}
	}
	t.Errorf("%s contains none of %q", what, subs)
}

// hasLine reports whether some line of text contains all of subs.
func hasLine(text string, subs ...string) bool {
	for line := range strings.Lines(text) {
		all := true
		for _, s := range subs {
			all = all && strings.Contains(line, s)
		}
		if all {
			return true
		}
	}
	return false
}
