package embed

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func shortTTLs(t *testing.T) {
	t.Helper()
	h, f, i := healthyTTL, failedTTL, infoTTL
	healthyTTL, failedTTL, infoTTL = 50*time.Millisecond, 20*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { healthyTTL, failedTTL, infoTTL = h, f, i })
}

// A dead server is noticed within the TTL and a revived one is picked up
// again without restarting the client.
func TestHealthNoticesDeathAndRecovery(t *testing.T) {
	shortTTLs(t)
	var up atomic.Bool
	up.Store(true)
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		if !up.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"model":"x","vision":true,"dims":4,"device":"cuda"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "", 4, time.Second)
	ctx := context.Background()
	if err := c.Healthy(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Healthy(ctx); err != nil || probes.Load() != 1 {
		t.Fatalf("a fresh success must be cached: %v, %d probes", err, probes.Load())
	}
	up.Store(false)
	time.Sleep(60 * time.Millisecond)
	if err := c.Healthy(ctx); err == nil {
		t.Fatal("a dead server must be noticed after the TTL")
	}
	if c.LastError() == nil {
		t.Fatal("LastError should explain the failure")
	}
	up.Store(true)
	time.Sleep(30 * time.Millisecond)
	if err := c.Healthy(ctx); err != nil {
		t.Fatalf("a revived server must be picked up: %v", err)
	}
	if info, err := c.Info(ctx); err != nil || info.Device != "cuda" {
		t.Fatalf("info after recovery: %+v %v", info, err)
	}
}

func TestErrorClassification(t *testing.T) {
	for _, c := range []struct {
		err                    error
		transient, unreachable bool
	}{
		{&StatusError{503, "out of memory"}, true, false},
		{&StatusError{400, "bad image"}, false, false},
		{&InputError{0, errors.New("no such file")}, false, false},
		{errors.New("dial tcp 127.0.0.1:1: connection refused"), true, true},
		{ErrNoVision, false, false},
		{nil, false, false},
	} {
		if Transient(c.err) != c.transient || Unreachable(c.err) != c.unreachable {
			t.Errorf("%v: transient=%v unreachable=%v", c.err, Transient(c.err), Unreachable(c.err))
		}
	}
	// The client reports server errors as StatusError and the server as
	// alive (it answered).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/embeddings" {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":{"message":"CUDA out of memory","type":"out_of_memory"}}`))
			return
		}
		w.Write([]byte(`{"model":"x","dims":4}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "", 4, time.Second)
	_, err := c.Embed(context.Background(), []Input{{Text: "a"}})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 503 || !strings.Contains(se.Msg, "out of memory") || !Transient(err) || Unreachable(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestServerLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embed-server.lock")
	if _, held := LockHolder(path); held {
		t.Fatal("fresh lock must be free")
	}
	release, err := tryLock(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if pid, held := LockHolder(path); !held || pid <= 0 {
		t.Fatalf("held lock: pid=%d held=%v", pid, held)
	}
	if _, err := tryLock(path, true); !errors.Is(err, errLocked) {
		t.Fatalf("second lock: %v", err)
	}
	// Serve refuses to start a second server while the lock is held.
	err = Serve(context.Background(), nil, ServeOptions{Sidecar: SidecarOptions{CacheDir: filepath.Dir(path)}})
	if !errors.Is(err, ErrServerRunning) {
		t.Fatalf("Serve with lock held: %v", err)
	}
	release()
	if _, held := LockHolder(path); held {
		t.Fatal("released lock must be free")
	}
}

// Ensure returns at once for a healthy server and never starts a second
// one while a server holds the lock (it is still loading).
func TestEnsureReusesAndWaits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"x","dims":4}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := Ensure(context.Background(), New(srv.URL, "", 4, time.Second), EnsureOptions{CacheDir: dir, Exe: "/nonexistent"}); err != nil {
		t.Fatalf("healthy server: %v", err)
	}
	release, err := tryLock(ServerLockPath(dir), true)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	dead := New("http://127.0.0.1:1", "", 4, time.Second)
	// Exe does not exist: spawning would fail loudly, so ErrStarting proves
	// Ensure only waited on the lock holder.
	if err := Ensure(context.Background(), dead, EnsureOptions{CacheDir: dir, Exe: "/nonexistent"}); !errors.Is(err, ErrStarting) {
		t.Fatalf("server starting elsewhere: %v", err)
	}
}

func TestUnitIsPortable(t *testing.T) {
	u := Unit(UnitOptions{Exe: "/home/u/.local/bin/modon", UVDir: "/home/u/.local/bin", Home: "/home/u"})
	for _, want := range []string{"ExecStart=%h/.local/bin/modon embed-server", "Environment=PATH=%h/.local/bin:", "Restart=on-failure"} {
		if !strings.Contains(u, want) {
			t.Errorf("unit lacks %q:\n%s", want, u)
		}
	}
	// Started on demand only: never at login, never revived after an idle exit.
	for _, bad := range []string{"WantedBy=", "Restart=always"} {
		if strings.Contains(u, bad) {
			t.Errorf("unit must not contain %q:\n%s", bad, u)
		}
	}
	if strings.Contains(u, "/home/u") {
		t.Errorf("unit must not hard-code the home directory:\n%s", u)
	}
}
