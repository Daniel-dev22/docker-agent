package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

func jobLog(j *Job) []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.ringBuffer)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func terminal(j *Job) bool {
	switch j.snapshot().State {
	case JobCompleted, JobFailed, JobCancelled:
		return true
	}
	return false
}

// H2: every project-scoped job waits for its project's lock — its log says for
// what — while a job on another project does not; the wait honours cancellation
// and the op timeout.
func TestProjectJobsQueueBehindTheProjectLock(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	locks := e.a.reg.eng.locks
	ctx := context.Background()

	for _, op := range []string{opComposeUp, opComposeDown, opComposePull, opComposeRestart, opComposeRecreate, opComposeUpdate} {
		t.Run(op, func(t *testing.T) {
			// "stack" is neither registered nor running: it has no directory, so it is
			// locked by name.
			release, err := locks.acquire(ctx, "name:stack", "the test's holder", nil)
			must(t, err)
			t.Cleanup(release) // a failure below must not leave the next case waiting forever
			j := e.a.reg.start(ctx, JobRequest{Operation: op, Project: "stack"})
			waitFor(t, "the queued line", func() bool {
				return slices.Contains(jobLog(j), "queued: waiting for the test's holder, which is changing the same project directory")
			})
			time.Sleep(50 * time.Millisecond)
			if terminal(j) {
				t.Fatalf("%s ran while the project was locked: %v", op, jobLog(j))
			}

			other := e.a.reg.start(ctx, JobRequest{Operation: op, Project: "another"})
			waitFor(t, "a job on another project", func() bool { return terminal(other) })

			release()
			waitFor(t, "the queued job to run", func() bool { return terminal(j) })
			if msg := j.snapshot().ErrorMsg; !strings.Contains(msg, "compose backend unavailable") {
				t.Fatalf("after the lock was released the job should have run its op, got %q", msg)
			}
		})
	}

	t.Run("cancelled-while-queued", func(t *testing.T) {
		release, err := locks.acquire(ctx, "name:stack", "the test's holder", nil)
		must(t, err)
		defer release()
		j := e.a.reg.start(ctx, JobRequest{Operation: opComposeUpdate, Project: "stack"})
		waitFor(t, "the queued line", func() bool { return len(jobLog(j)) > 0 })
		if !e.a.reg.cancelJob(j.ID) {
			t.Fatal("a queued job must be cancellable")
		}
		waitFor(t, "the cancelled job to leave the queue", func() bool {
			return slices.Contains(jobLog(j), "cancelled while queued")
		})
		if st := j.snapshot().State; st != JobCancelled {
			t.Fatalf("state %s", st)
		}
	})

	t.Run("gives-up-after-the-op-timeout", func(t *testing.T) {
		release, err := locks.acquire(ctx, "name:stack", "the test's holder", nil)
		must(t, err)
		defer release()
		e.a.reg.eng.composeOpTimeout = 100 * time.Millisecond
		defer func() { e.a.reg.eng.composeOpTimeout = 30 * time.Minute }()
		j := e.a.reg.start(ctx, JobRequest{Operation: opComposeUp, Project: "stack"})
		waitFor(t, "the queued job to give up", func() bool { return terminal(j) })
		if msg := j.snapshot().ErrorMsg; !strings.Contains(msg, "gave up after 100ms queued") {
			t.Fatalf("got %q", msg)
		}
	})
}

// A register or copy onto a project another change holds waits briefly, then
// answers 409 project_busy — retryable — having written nothing.
func TestRequestsAnswerBusyWhileTheProjectIsLocked(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	requestLockWait = 50 * time.Millisecond
	defer func() { requestLockWait = 10 * time.Second }()
	ctx := context.Background()

	if status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{
		"name": "srcproj", "files": map[string]string{"compose.yaml": "services:\n  app:\n    image: nginx:alpine\n"},
	}); status != http.StatusOK {
		t.Fatalf("fixture register: %d %v", status, body)
	}

	release, err := e.a.reg.eng.locks.acquire(ctx, projectLockKey(ProjectEntry{Name: "busyproj", WorkingDir: filepath.Join(e.root, "busyproj")}), "job 42 (update)", nil)
	must(t, err)
	status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{
		"name": "busyproj", "replace": true, "files": map[string]string{"compose.yaml": "services: {}\n"},
	})
	release()
	if status != http.StatusConflict || body["code"] != "project_busy" || body["retryable"] != true || body["holder"] != "job 42 (update)" {
		t.Fatalf("register onto a locked project: %d %v", status, body)
	}
	if _, err := os.Stat(filepath.Join(e.root, "busyproj")); !os.IsNotExist(err) {
		t.Fatalf("a busy register wrote its files (%v)", err)
	}

	src, _ := e.a.projects.get("srcproj")
	release, err = e.a.reg.eng.locks.acquire(ctx, projectLockKey(src), "job 43 (update)", nil)
	must(t, err)
	status, body = e.do(t, http.MethodPost, "/v1/projects/srcproj/copy", map[string]any{"new_name": "dstproj"})
	release()
	if status != http.StatusConflict || body["code"] != "project_busy" {
		t.Fatalf("copy of a locked source: %d %v", status, body)
	}
	if _, err := os.Stat(filepath.Join(e.root, "dstproj")); !os.IsNotExist(err) {
		t.Fatalf("a busy copy wrote its files (%v)", err)
	}

	// Positive control: the same requests go through once the project is free.
	if status, body := e.do(t, http.MethodPost, "/v1/projects/srcproj/copy", map[string]any{"new_name": "dstproj"}); status != http.StatusOK {
		t.Fatalf("copy once free: %d %v", status, body)
	}
}

// G: an image ID is not a reference. The check says so instead of reporting a
// registry 401 for a repository called "sha256"; an update refuses with the way
// out; an override to one is refused at the door.
func TestImageIDsAreNamedAsSuch(t *testing.T) {
	const id = "sha256:15dc409d48a2a475ef6e54b26e211a13a97f5ea5d0b7f20bf2ee22c37ea33237"

	ic := &imageChecker{overrides: map[string]strategyOverride{}, arch: "amd64"}
	res := ic.checkUnit(t.Context(), ContainerStatus{Name: "esphome", Image: id, ComposeProject: "esphome", ComposeService: "esphome"}, imageInfo{})
	if res.ImageStatus != statusUnknown || !strings.Contains(res.Error, "local image ID") || strings.Contains(res.Error, "HTTP") {
		t.Fatalf("check: %+v", res)
	}

	r := &registryResolver{e: &engine{}}
	project := projectWith(map[string]string{"esphome": id})
	if _, err := r.plan(t.Context(), project, func(string) {}); err == nil || !strings.Contains(err.Error(), "override_image") {
		t.Fatalf("update plan: %v", err)
	}

	e := newCapEnv(t, testSelfID, defaultContainers)
	if status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{
		"name": "esphome", "files": map[string]string{"compose.yaml": "services:\n  esphome:\n    image: ${CURRENT_ESPHOME_IMAGE}\n"},
	}); status != http.StatusOK {
		t.Fatalf("fixture register: %d %v", status, body)
	}
	status, body := e.do(t, http.MethodPost, "/v1/projects/esphome/op", map[string]any{"op": "update", "override_image": id})
	if status != http.StatusBadRequest || body["code"] != "invalid_override_image" {
		t.Fatalf("override to an image ID: %d %v", status, body)
	}
}

func projectWith(images map[string]string) *types.Project {
	p := &types.Project{Name: "p", Services: types.Services{}}
	for name, img := range images {
		p.Services[name] = types.ServiceConfig{Name: name, Image: img}
	}
	return p
}
