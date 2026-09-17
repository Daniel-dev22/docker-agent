package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

const composeA = "services:\n  app:\n    image: nginx:alpine\n"

// The reproducer: `foo` registered path-only onto `bar`'s directory would let a
// change through one name bypass the other's lock and overwrite its files. Two
// entries must not share a working directory — spelled differently or through a
// symlink either.
func TestRegisterRefusesASharedWorkingDir(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	if status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{"name": "bar", "files": map[string]string{"compose.yaml": composeA}}); status != http.StatusOK {
		t.Fatalf("fixture register: %d %v", status, body)
	}
	barDir := filepath.Join(e.root, "bar")
	barCompose, err := os.ReadFile(filepath.Join(barDir, "compose.yaml"))
	must(t, err)
	must(t, os.Symlink(barDir, filepath.Join(e.root, "bar-link")))

	for _, dir := range []string{barDir, barDir + "/", filepath.Join(e.root, "bar-link")} {
		status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{"name": "foo", "working_dir": dir})
		if status != http.StatusConflict || body["code"] != "working_dir_in_use" || body["project"] != "bar" {
			t.Fatalf("path-only register onto %s: %d %v", dir, status, body)
		}
	}
	if _, ok := e.a.projects.get("foo"); ok {
		t.Fatal("a refused register was recorded")
	}

	// A copy onto a name whose directory another project already owns.
	quxDir := filepath.Join(e.root, "qux")
	must(t, os.MkdirAll(quxDir, 0o755))
	must(t, os.WriteFile(filepath.Join(quxDir, "compose.yaml"), []byte(composeA), 0o644))
	if status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{"name": "owner", "working_dir": quxDir}); status != http.StatusOK {
		t.Fatalf("fixture path-only register: %d %v", status, body)
	}
	quxBefore, err := os.ReadFile(filepath.Join(quxDir, "compose.yaml"))
	must(t, err)
	status, body := e.do(t, http.MethodPost, "/v1/projects/bar/copy", map[string]any{"new_name": "qux"})
	if status != http.StatusConflict || body["code"] != "working_dir_in_use" || body["project"] != "owner" {
		t.Fatalf("copy onto an owned directory: %d %v", status, body)
	}
	if after, _ := os.ReadFile(filepath.Join(quxDir, "compose.yaml")); string(after) != string(quxBefore) {
		t.Error("a refused copy overwrote the other project's files")
	}

	// Positive controls: a project may re-point to its own directory, and a new
	// directory is free.
	if status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{"name": "bar", "replace": true, "working_dir": barDir}); status != http.StatusOK {
		t.Fatalf("re-registering bar onto its own directory: %d %v", status, body)
	}
	if got, _ := os.ReadFile(filepath.Join(barDir, "compose.yaml")); string(got) != string(barCompose) {
		t.Error("bar's files changed")
	}
}

// A duplicate that predates the refusal (a projects.json from an older release)
// still cannot bypass the lock: it is keyed by directory, not name.
func TestProjectLockIsKeyedByDirectory(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	requestLockWait = 50 * time.Millisecond
	defer func() { requestLockWait = 10 * time.Second }()
	if status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{"name": "bar", "files": map[string]string{"compose.yaml": composeA}}); status != http.StatusOK {
		t.Fatalf("fixture register: %d %v", status, body)
	}
	barDir := filepath.Join(e.root, "bar")
	must(t, os.Symlink(barDir, filepath.Join(e.root, "bar-link")))
	// Bypasses the handler, as an old projects.json would.
	must(t, e.a.projects.register(ProjectEntry{Name: "foo", WorkingDir: filepath.Join(e.root, "bar-link")}))
	foo, _ := e.a.projects.get("foo")
	bar, _ := e.a.projects.get("bar")
	if projectLockKey(foo) != projectLockKey(bar) {
		t.Fatalf("two names for one directory lock differently: %q vs %q", projectLockKey(foo), projectLockKey(bar))
	}

	release, err := e.a.reg.eng.locks.acquire(context.Background(), projectLockKey(foo), "job 7 (update of foo)", nil)
	must(t, err)
	before, _ := os.ReadFile(filepath.Join(barDir, "compose.yaml"))
	status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{"name": "bar", "replace": true, "files": map[string]string{"compose.yaml": "services: {}\n"}})
	if status != http.StatusConflict || body["code"] != "project_busy" || body["holder"] != "job 7 (update of foo)" {
		release()
		t.Fatalf("register through the other name while foo is locked: %d %v", status, body)
	}
	if after, _ := os.ReadFile(filepath.Join(barDir, "compose.yaml")); string(after) != string(before) {
		t.Error("the register overwrote files while the other name held the directory")
	}
	j := e.a.reg.start(context.Background(), JobRequest{Operation: opComposeUp, Project: "bar"})
	waitFor(t, "a job on bar to queue behind foo", func() bool {
		return slices.Contains(jobLog(j), "queued: waiting for job 7 (update of foo), which is changing the same project directory")
	})
	release()
	waitFor(t, "the queued job to run", func() bool { return terminal(j) })
}

// Duplicates already in projects.json are found at load and reported in readiness,
// and auto-adoption does not create new ones.
func TestSharedWorkingDirsAreReported(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "stack")
	must(t, os.MkdirAll(dir, 0o755))
	must(t, os.Symlink(dir, filepath.Join(root, "alias")))
	path := filepath.Join(root, "projects.json")
	data, _ := json.Marshal([]ProjectEntry{
		{Name: "one", WorkingDir: dir},
		{Name: "two", WorkingDir: filepath.Join(root, "alias")},
		{Name: "three", WorkingDir: filepath.Join(root, "elsewhere")},
	})
	must(t, os.WriteFile(path, data, 0o600))
	reg := newComposeRegistry(path, root)
	must(t, reg.load())
	shared := reg.sharedWorkingDirs()
	if len(shared) != 1 || !slices.Equal(sortedCopy(shared[canonicalDir(dir)]), []string{"one", "two"}) {
		t.Fatalf("shared = %v", shared)
	}

	reg.enrichFromLive([]ComposeProject{{Name: "adhoc", WorkingDir: dir}, {Name: "fresh", WorkingDir: filepath.Join(root, "fresh")}})
	if _, ok := reg.get("adhoc"); ok {
		t.Error("a running project on a registered directory was adopted under a second name")
	}
	if _, ok := reg.get("fresh"); !ok {
		t.Error("a running project on a free directory must still be adopted")
	}

	must(t, reg.deregister("two"))
	if n := len(reg.sharedWorkingDirs()); n != 0 {
		t.Errorf("after deregistering one of the pair, %d shared dirs remain", n)
	}

	e := newCapEnv(t, testSelfID, defaultContainers)
	must(t, e.a.projects.register(ProjectEntry{Name: "x", WorkingDir: filepath.Join(e.root, "d")}))
	must(t, e.a.projects.register(ProjectEntry{Name: "y", WorkingDir: filepath.Join(e.root, "d")}))
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	var ready struct {
		Projects struct {
			SharedWorkingDirs map[string][]string `json:"shared_working_dirs"`
		} `json:"projects"`
	}
	must(t, json.Unmarshal(w.Body.Bytes(), &ready))
	if got := ready.Projects.SharedWorkingDirs[canonicalDir(filepath.Join(e.root, "d"))]; !slices.Equal(sortedCopy(got), []string{"x", "y"}) {
		t.Fatalf("readiness: %s", w.Body.String())
	}
}

func sortedCopy(s []string) []string {
	return slices.Sorted(slices.Values(s))
}

// A retryable refusal is not the request's answer and is never recorded under its
// Idempotency-Key: once the project frees, the same key goes through, once.
func TestRetryableRefusalsAreNotRecorded(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	withIdempotency(t, e, t.TempDir())
	requestLockWait = 50 * time.Millisecond
	defer func() { requestLockWait = 10 * time.Second }()

	body := `{"name":"keyed","files":{"compose.yaml":"services:\n  app:\n    image: nginx:alpine\n"}}`
	key := projectLockKey(ProjectEntry{Name: "keyed", WorkingDir: filepath.Join(e.root, "keyed")})
	release, err := e.a.reg.eng.locks.acquire(context.Background(), key, "job 9 (update)", nil)
	must(t, err)
	busy := e.doKeyed(t, http.MethodPost, "/v1/projects", "k-busy-register", body)
	release()
	if busy.status != http.StatusConflict || busy.code() != "project_busy" {
		t.Fatalf("keyed register while locked: %d %s", busy.status, busy.body)
	}
	if n := idemRows(t, e.a.events.DB(), "key = 'k-busy-register'"); n != 0 {
		t.Fatalf("the retryable refusal was recorded (%d rows)", n)
	}
	again := e.doKeyed(t, http.MethodPost, "/v1/projects", "k-busy-register", body)
	if again.status != http.StatusOK || again.replayed == "true" {
		t.Fatalf("the same key after the project freed: %d %s replayed=%q", again.status, again.body, again.replayed)
	}
	if replay := e.doKeyed(t, http.MethodPost, "/v1/projects", "k-busy-register", body); replay.replayed != "true" || replay.status != http.StatusOK {
		t.Fatalf("the successful answer must now be the recorded one: %d replayed=%q", replay.status, replay.replayed)
	}

	// The contract, for every code allowed to say "retryable": its refusal releases
	// the claim, exactly like a 5xx.
	for code := range retryableCodes {
		r := gin.New()
		calls := 0
		r.POST("/probe", e.a.idempotent(), func(c *gin.Context) {
			calls++
			refuse(c, http.StatusConflict, code, "try again", gin.H{"retryable": true})
		})
		for range 2 {
			req := httptest.NewRequest(http.MethodPost, "/probe", nil)
			req.Header.Set("Idempotency-Key", "k-"+code)
			r.ServeHTTP(httptest.NewRecorder(), req)
		}
		if calls != 2 || idemRows(t, e.a.events.DB(), "key = ?", "k-"+code) != 0 {
			t.Errorf("%s: a retryable refusal was recorded (handler ran %d times)", code, calls)
		}
	}
}
