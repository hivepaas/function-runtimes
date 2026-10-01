//go:build conformance

package conformance

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// images are what the suite built for one runtime.
type images struct {
	// runtime is the runtime's image, and build its build image (compiled
	// runtimes only).
	runtime, build string
	// fixture is the fixture function, ready to serve and invoke.
	fixture string
	// source is the fixture not built yet, in the build image (compiled
	// runtimes only): what a test run starts from.
	source string
}

func buildImages(t *testing.T, rt runtimeDef) images {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := filepath.Join(root, "runtimes", rt.name, "Dockerfile")
	fixtureDir := filepath.Join(root, "conformance", "fixtures", rt.name)
	fixtureDockerfile := filepath.Join(fixtureDir, "Dockerfile")
	imgs := images{
		runtime: "hp-conformance/runtime-" + rt.name,
		fixture: "hp-conformance/fixture-" + rt.name,
	}
	// The fixture builds without a network: everything it needs is in it.
	fixtureArgs := []string{"--network", "none", "--build-arg", "RUNTIME=" + imgs.runtime}

	if rt.compiled {
		imgs.build = imgs.runtime + "-build"
		imgs.source = imgs.fixture + "-source"
		buildImage(t, imgs.runtime, dockerfile, root, "--target", "run")
		buildImage(t, imgs.build, dockerfile, root, "--target", "build")
		fixtureArgs = append(fixtureArgs, "--build-arg", "BUILD_RUNTIME="+imgs.build)
		buildImage(t, imgs.source, fixtureDockerfile, fixtureDir, append(fixtureArgs, "--target", "source")...)
	} else {
		buildImage(t, imgs.runtime, dockerfile, root)
	}
	buildImage(t, imgs.fixture, fixtureDockerfile, fixtureDir, fixtureArgs...)
	return imgs
}

func buildImage(t *testing.T, tag, dockerfile, contextDir string, args ...string) {
	t.Helper()
	docker(t, append(append([]string{"build", "-t", tag, "-f", dockerfile}, args...), contextDir)...)
	if os.Getenv("KEEP_IMAGES") == "" {
		t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", tag).Run() })
	}
}

// result is what a docker command did.
type result struct {
	stdout, stderr string
	code           int
	took           time.Duration
}

// dockerRun runs the docker command line with stdin. A failure to start
// docker fails the test; the command's own exit code is in the result.
func dockerRun(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	cmd := exec.Command("docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(stdin), &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	r := result{stdout: stdout.String(), stderr: stderr.String(), took: time.Since(start)}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		r.code = exitErr.ExitCode()
	case err != nil:
		t.Fatalf("docker %s: %v", strings.Join(args, " "), err)
	}
	return r
}

// docker runs the docker command line and returns its output. The test fails
// when the command does.
func docker(t *testing.T, args ...string) string {
	t.Helper()
	r := dockerRun(t, "", args...)
	if r.code != 0 {
		t.Fatalf("docker %s: exit %d\n%s\n%s", strings.Join(args, " "), r.code, lastLines(r.stdout), lastLines(r.stderr))
	}
	return r.stdout
}

func lastLines(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 30 {
		lines = lines[len(lines)-30:]
	}
	return strings.Join(lines, "\n")
}

// container is a fixture serving on a port of this host.
type container struct {
	id  string
	url string
}

// startServe runs image's serve with env, and waits for it to answer.
func startServe(t *testing.T, image string, env ...string) *container {
	t.Helper()
	args := []string{"run", "-d", "-p", "127.0.0.1::8080"}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	id := strings.TrimSpace(docker(t, append(args, image)...))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	address := strings.TrimSpace(docker(t, "port", id, "8080/tcp"))
	c := &container{id: id, url: "http://" + strings.Split(address, "\n")[0]}

	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(c.url + "/_hivepaas/health")
		if err == nil {
			_ = resp.Body.Close()
			return c
		}
		if time.Now().After(deadline) {
			stdout, stderr := c.logs(t)
			t.Fatalf("%s did not answer: %v\n%s\n%s", image, err, stdout, stderr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// response is what a call answered.
type response struct {
	status int
	header http.Header
	body   []byte
	err    error // of a call made in the background
}

func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("the body %q is not a JSON object: %v", r.body, err)
	}
	return v
}

var client = &http.Client{Timeout: 15 * time.Second}

func (c *container) call(t *testing.T, method, path string, body []byte, headers map[string]string) response {
	t.Helper()
	r := c.do(method, path, bytes.NewReader(body), headers)
	if r.err != nil {
		t.Fatalf("%s %s: %v", method, path, r.err)
	}
	return r
}

// chunked sends body without a length, in chunks, as a proxy streaming a
// request does.
func (c *container) chunked(t *testing.T, path string, body []byte) response {
	t.Helper()
	r := c.do(http.MethodPost, path, io.MultiReader(bytes.NewReader(body)), nil)
	if r.err != nil {
		t.Fatalf("POST %s: %v", path, r.err)
	}
	return r
}

// background makes a GET in the background; its response comes on the channel.
func (c *container) background(path string) <-chan response {
	done := make(chan response, 1)
	go func() { done <- c.do(http.MethodGet, path, http.NoBody, nil) }()
	return done
}

func (c *container) do(method, path string, body io.Reader, headers map[string]string) response {
	req, err := http.NewRequest(method, c.url+path, body)
	if err != nil {
		return response{err: err}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return response{err: err}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, header: resp.Header, body: b, err: err}
}

func (c *container) get(t *testing.T, path string) response {
	t.Helper()
	return c.call(t, http.MethodGet, path, nil, nil)
}

func (c *container) logs(t *testing.T) (stdout, stderr string) {
	t.Helper()
	r := dockerRun(t, "", "logs", c.id)
	return r.stdout, r.stderr
}

// hasLog says whether a line of the container's log, on standard output or
// standard error, satisfies match.
func (c *container) hasLog(t *testing.T, stream string, match func(line string) bool) bool {
	t.Helper()
	stdout, stderr := c.logs(t)
	text := stdout
	if stream == "stderr" {
		text = stderr
	}
	return slices.ContainsFunc(strings.Split(text, "\n"), match)
}

// waitForLog waits for a line of the container's log that satisfies match, on
// standard output or standard error.
func (c *container) waitForLog(t *testing.T, stream string, match func(line string) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		stdout, stderr := c.logs(t)
		text := stdout
		if stream == "stderr" {
			text = stderr
		}
		for _, line := range strings.Split(text, "\n") {
			if match(line) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no such line on %s:\n%s", stream, text)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// jsonLine is the JSON log line of kind hp ("invocation", "log") for the call
// requestID.
func (c *container) jsonLine(t *testing.T, hp, requestID string) map[string]any {
	t.Helper()
	var found map[string]any
	c.waitForLog(t, "stdout", func(line string) bool {
		var v map[string]any
		if json.Unmarshal([]byte(line), &v) != nil || v["hp"] != hp || v["requestId"] != requestID {
			return false
		}
		found = v
		return true
	})
	return found
}
