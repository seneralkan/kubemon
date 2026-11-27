package logs

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/seneralkan/kubemon/internal/model"
)

// fakeStream is one follow request; the test writes log lines into w.
type fakeStream struct {
	pod    string
	opts   corev1.PodLogOptions
	w      *io.PipeWriter
	closed chan struct{}
}

type closeNotifier struct {
	*io.PipeReader
	once   sync.Once
	closed chan struct{}
}

func (c *closeNotifier) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.PipeReader.Close()
}

type fakeLogs struct {
	mu       sync.Mutex
	previous map[string]string // "pod/container" -> previous logs
	prevN    int               // previous requests served
	errs     map[string]error  // "pod/container" -> error for follow opens
	followN  int               // follow open attempts
	opened   chan *fakeStream
}

func newFake() *fakeLogs {
	return &fakeLogs{previous: map[string]string{}, errs: map[string]error{}, opened: make(chan *fakeStream, 100)}
}

func (f *fakeLogs) open(_ context.Context, pod string, o *corev1.PodLogOptions) (io.ReadCloser, error) {
	if o.Previous {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.prevN++
		return io.NopCloser(strings.NewReader(f.previous[pod+"/"+o.Container])), nil
	}
	f.mu.Lock()
	f.followN++
	err := f.errs[pod+"/"+o.Container]
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	r, w := io.Pipe()
	s := &fakeStream{pod: pod, opts: *o, w: w, closed: make(chan struct{})}
	f.opened <- s
	return &closeNotifier{PipeReader: r, closed: s.closed}, nil
}

func (f *fakeLogs) setErr(key string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[key] = err
}

func (f *fakeLogs) attempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.followN
}

func (f *fakeLogs) waitOpen(t *testing.T) *fakeStream {
	t.Helper()
	select {
	case s := <-f.opened:
		return s
	case <-time.After(2 * time.Second):
		t.Fatal("no stream opened")
		return nil
	}
}

func (f *fakeLogs) noOpen(t *testing.T) {
	t.Helper()
	select {
	case s := <-f.opened:
		t.Fatalf("unexpected stream %s/%s", s.pod, s.opts.Container)
	case <-time.After(50 * time.Millisecond):
	}
}

func recv(t *testing.T, ch <-chan Line) Line {
	t.Helper()
	select {
	case l, ok := <-ch:
		if !ok {
			t.Fatal("channel closed")
		}
		return l
	case <-time.After(2 * time.Second):
		t.Fatal("no line")
		return Line{}
	}
}

func noLine(t *testing.T, ch <-chan Line) {
	t.Helper()
	select {
	case l := <-ch:
		t.Fatalf("unexpected line %+v", l)
	case <-time.After(50 * time.Millisecond):
	}
}

func ts(sec int) string {
	return time.Date(2026, 1, 1, 0, 0, sec, 0, time.UTC).Format(time.RFC3339Nano)
}

func running(name string, restarts int32) model.Container {
	return model.Container{Name: name, State: "Running", RestartCount: restarts}
}

func pod(name string, cs ...model.Container) model.Pod {
	return model.Pod{Name: name, Namespace: "ns", Containers: cs}
}

func setup(t *testing.T, o Options) (*Follower, *fakeLogs, context.Context) {
	t.Helper()
	fk := newFake()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f := newFollower(fk.open, o)
	f.bo = backoff{retryMin: time.Millisecond, retryMax: 5 * time.Millisecond, quick: time.Second,
		errMin: 200 * time.Millisecond, errMax: 400 * time.Millisecond}
	return f, fk, ctx
}

func TestParseLine(t *testing.T) {
	tm, text, ok := parseLine("2026-01-01T00:00:01.123456789Z hello world\n")
	if !ok || text != "hello world" || tm.Nanosecond() != 123456789 {
		t.Fatalf("got %v %q %v", tm, text, ok)
	}
	tm, text, ok = parseLine("2026-01-01T00:00:01Z\r\n")
	if !ok || text != "" || tm.Second() != 1 {
		t.Fatalf("empty text: got %v %q %v", tm, text, ok)
	}
	before := time.Now()
	tm, text, ok = parseLine("no timestamp here\n")
	if ok || text != "no timestamp here" || tm.Before(before) {
		t.Fatalf("fallback: got %v %q %v", tm, text, ok)
	}
}

func TestLongLine(t *testing.T) {
	f, fk, ctx := setup(t, Options{})
	long := strings.Repeat("x", 1<<20)
	fk.previous["p/app"] = ts(1) + " " + long + "\n" + ts(2) + " tail" // last line without newline
	c := model.Container{Name: "app", State: "Waiting", RestartCount: 1, Last: &model.Termination{}}
	f.Sync(ctx, []model.Pod{pod("p", c)})

	if l := recv(t, f.Lines()); l.Text != long || !l.Previous {
		t.Fatalf("long line: len %d previous %v", len(l.Text), l.Previous)
	}
	if l := recv(t, f.Lines()); l.Text != "tail" {
		t.Fatalf("got %q", l.Text)
	}
}

func TestStartsOnlyRunningAppContainers(t *testing.T) {
	containers := []model.Container{
		running("app", 0),
		{Name: "proxy", Init: true, State: "Running"},
		{Name: "worker", State: "Waiting"},
		running("other", 0),
	}

	f, fk, ctx := setup(t, Options{TailLines: 5})
	f.Sync(ctx, []model.Pod{pod("p", containers...)})
	got := map[string]bool{}
	for range 2 {
		s := fk.waitOpen(t)
		got[s.opts.Container] = true
		if !s.opts.Follow || !s.opts.Timestamps || s.opts.TailLines == nil || *s.opts.TailLines != 5 {
			t.Fatalf("bad options %+v", s.opts)
		}
	}
	if !got["app"] || !got["other"] {
		t.Fatalf("streams %v", got)
	}
	fk.noOpen(t)

	f, fk, ctx = setup(t, Options{Container: "other"})
	f.Sync(ctx, []model.Pod{pod("p", containers...)})
	if s := fk.waitOpen(t); s.opts.Container != "other" {
		t.Fatalf("filter: got %s", s.opts.Container)
	}
	fk.noOpen(t)
}

func TestNoDuplicateStream(t *testing.T) {
	f, fk, ctx := setup(t, Options{})
	pods := []model.Pod{pod("p", running("app", 0))}
	f.Sync(ctx, pods)
	f.Sync(ctx, pods)
	s := fk.waitOpen(t)
	f.Sync(ctx, pods)
	fk.noOpen(t)

	go s.w.Write([]byte(ts(1) + " hi\n"))
	if l := recv(t, f.Lines()); l.Text != "hi" || l.Pod != "p" || l.Container != "app" || l.Time.Second() != 1 {
		t.Fatalf("got %+v", l)
	}
}

func TestStopsRemovedPods(t *testing.T) {
	f, fk, ctx := setup(t, Options{})
	f.Sync(ctx, []model.Pod{pod("a", running("app", 0)), pod("b", running("app", 0))})
	streams := map[string]*fakeStream{}
	for range 2 {
		s := fk.waitOpen(t)
		streams[s.pod] = s
	}

	f.Sync(ctx, []model.Pod{pod("a", running("app", 0))})
	select {
	case <-streams["b"].closed:
	case <-time.After(2 * time.Second):
		t.Fatal("stream of removed pod not closed")
	}
	select {
	case <-streams["a"].closed:
		t.Fatal("stream of listed pod closed")
	default:
	}
}

func TestPreviousOncePerRestart(t *testing.T) {
	f, fk, ctx := setup(t, Options{TailLines: 10})
	fk.previous["p/app"] = ts(1) + " panic: boom\n"
	crashed := func(restarts int32) []model.Pod {
		c := model.Container{Name: "app", State: "Waiting", Reason: "CrashLoopBackOff", RestartCount: restarts, Last: &model.Termination{Reason: "Error"}}
		return []model.Pod{pod("p", c)}
	}

	// First sighting of a pod that already restarted.
	f.Sync(ctx, crashed(3))
	if l := recv(t, f.Lines()); !l.Previous || l.Text != "panic: boom" {
		t.Fatalf("got %+v", l)
	}
	f.Sync(ctx, crashed(3))
	f.Sync(ctx, crashed(3))
	noLine(t, f.Lines())

	f.Sync(ctx, crashed(4))
	if l := recv(t, f.Lines()); !l.Previous {
		t.Fatalf("got %+v", l)
	}
	noLine(t, f.Lines())

	fk.mu.Lock()
	defer fk.mu.Unlock()
	if fk.prevN != 2 {
		t.Fatalf("previous fetched %d times, want 2", fk.prevN)
	}
}

func TestNoPreviousWithoutRestart(t *testing.T) {
	f, fk, ctx := setup(t, Options{})
	f.Sync(ctx, []model.Pod{pod("p", running("app", 0))})
	fk.waitOpen(t)
	fk.mu.Lock()
	defer fk.mu.Unlock()
	if fk.prevN != 0 {
		t.Fatalf("previous fetched %d times", fk.prevN)
	}
}

// syncUntilOpen calls Sync until the follower (re)opens a stream: a dropped
// stream is forgotten asynchronously.
func syncUntilOpen(t *testing.T, f *Follower, fk *fakeLogs, ctx context.Context, pods []model.Pod) *fakeStream {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		f.Sync(ctx, pods)
		select {
		case s := <-fk.opened:
			return s
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("stream not reopened")
		}
	}
}

func TestResumeAfterDrop(t *testing.T) {
	f, fk, ctx := setup(t, Options{TailLines: 20})
	pods := []model.Pod{pod("p", running("app", 0))}
	f.Sync(ctx, pods)
	s := fk.waitOpen(t)
	go func() {
		s.w.Write([]byte(ts(1) + " a\n" + ts(2) + " b\n"))
		s.w.Close() // connection dropped
	}()
	recv(t, f.Lines())
	recv(t, f.Lines())

	// Same instance: reopened without a Sync, resuming from the last line
	// without repeating it. Syncs meanwhile start no second stream.
	f.Sync(ctx, pods)
	f.Sync(ctx, pods)
	s = fk.waitOpen(t)
	fk.noOpen(t)
	at := func(sec int) time.Time { return time.Date(2026, 1, 1, 0, 0, sec, 0, time.UTC) }
	if s.opts.SinceTime == nil || !s.opts.SinceTime.Time.Equal(at(2)) || s.opts.TailLines != nil {
		t.Fatalf("resume options %+v", s.opts)
	}
	go func() {
		s.w.Write([]byte(ts(2) + " b\n" + ts(3) + " c\n"))
		s.w.Close()
	}()
	if l := recv(t, f.Lines()); l.Text != "c" {
		t.Fatalf("got %q, want c", l.Text)
	}

	// Container exited: its stream ends at once with nothing new. After
	// maxFruitless such reopens run gives up.
	for range maxFruitless {
		s = fk.waitOpen(t)
		if s.opts.SinceTime == nil || !s.opts.SinceTime.Time.Equal(at(3)) {
			t.Fatalf("resume options %+v", s.opts)
		}
		go func() {
			s.w.Write([]byte(ts(3) + " c\n")) // already seen
			s.w.Close()
		}()
	}
	fk.noOpen(t)
	noLine(t, f.Lines())

	// Container restarted: Sync starts fresh with the tail.
	pods = []model.Pod{pod("p", running("app", 1))}
	s = syncUntilOpen(t, f, fk, ctx, pods)
	if s.opts.SinceTime != nil || s.opts.TailLines == nil || *s.opts.TailLines != 20 {
		t.Fatalf("restart options %+v", s.opts)
	}
}

func TestOpenErrorOnceWithBackoff(t *testing.T) {
	f, fk, ctx := setup(t, Options{})
	fk.setErr("p/app", apierrors.NewBadRequest("container app is waiting to start"))
	pods := []model.Pod{pod("p", running("app", 0))}
	f.Sync(ctx, pods)
	if l := recv(t, f.Lines()); !l.Err || l.Pod != "p" || l.Container != "app" ||
		l.Text != "logs unavailable: container app is waiting to start" {
		t.Fatalf("got %+v", l)
	}

	// Not retried before the backoff, however often Sync is called.
	for range 10 {
		f.Sync(ctx, pods)
		time.Sleep(5 * time.Millisecond)
	}
	if n := fk.attempts(); n != 1 {
		t.Fatalf("%d open attempts, want 1", n)
	}

	// Retried after it, but the same error is not emitted again.
	deadline := time.Now().Add(2 * time.Second)
	for fk.attempts() < 2 && time.Now().Before(deadline) {
		f.Sync(ctx, pods)
		time.Sleep(10 * time.Millisecond)
	}
	if n := fk.attempts(); n != 2 {
		t.Fatalf("%d open attempts, want 2", n)
	}
	noLine(t, f.Lines())

	// Recovers once the error goes away.
	fk.setErr("p/app", nil)
	s := syncUntilOpen(t, f, fk, ctx, pods)
	go s.w.Write([]byte(ts(1) + " hi\n"))
	if l := recv(t, f.Lines()); l.Err || l.Text != "hi" {
		t.Fatalf("got %+v", l)
	}
}

func TestTransientOpenErrorRetriedInRun(t *testing.T) {
	f, fk, ctx := setup(t, Options{})
	fk.setErr("p/app", errors.New("connection refused"))
	f.Sync(ctx, []model.Pod{pod("p", running("app", 0))})
	if l := recv(t, f.Lines()); !l.Err || l.Text != "logs unavailable: connection refused" {
		t.Fatalf("got %+v", l)
	}
	deadline := time.Now().Add(2 * time.Second)
	for fk.attempts() < maxFruitless && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	noLine(t, f.Lines()) // retries done; no repeated line
	if n := fk.attempts(); n != maxFruitless {
		t.Fatalf("%d open attempts, want %d", n, maxFruitless)
	}
}

func TestForbiddenStopsAll(t *testing.T) {
	f, fk, ctx := setup(t, Options{})
	f.bo.errMin, f.bo.errMax = time.Millisecond, time.Millisecond // only forbidden may stop retries
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "pods/log"}, "a", errors.New("denied"))
	fk.setErr("a/app", forbidden)
	fk.setErr("b/app", forbidden)
	pods := []model.Pod{pod("a", running("app", 0)), pod("b", running("app", 0))}
	f.Sync(ctx, pods)
	if l := recv(t, f.Lines()); !l.Err || !strings.HasPrefix(l.Text, "logs unavailable: ") || !strings.Contains(l.Text, "forbidden") {
		t.Fatalf("got %+v", l)
	}
	noLine(t, f.Lines()) // once for the whole follower
	n := fk.attempts()

	pods = append(pods, pod("c", running("app", 0)))
	for range 5 {
		f.Sync(ctx, pods)
		time.Sleep(5 * time.Millisecond)
	}
	fk.noOpen(t)
	if got := fk.attempts(); got != n {
		t.Fatalf("%d open attempts after forbidden, want %d", got, n)
	}
}

func TestTailLines(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want *int64
	}{{-1, nil}, {0, new(int64(0))}, {5, new(int64(5))}} {
		f, fk, ctx := setup(t, Options{TailLines: tc.in})
		f.Sync(ctx, []model.Pod{pod("p", running("app", 0))})
		got := fk.waitOpen(t).opts.TailLines
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Fatalf("TailLines %d: got %v", tc.in, got)
		}
	}
}

func TestClosesAfterCancel(t *testing.T) {
	f, fk, ctx := setup(t, Options{})
	ctx, cancel := context.WithCancel(ctx)
	f.Sync(ctx, []model.Pod{pod("p", running("app", 0))})
	s := fk.waitOpen(t) // stays open with nothing written
	cancel()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-f.Lines():
			if !ok {
				<-s.closed
				f.Sync(ctx, []model.Pod{pod("q", running("app", 0))}) // no-op after close
				fk.noOpen(t)
				return
			}
		case <-deadline:
			t.Fatal("channel not closed")
		}
	}
}

func TestLinesBeforeSync(t *testing.T) {
	f, _, _ := setup(t, Options{})
	if f.Lines() == nil || f.Lines() != f.Lines() {
		t.Fatal("Lines must return the same non-nil channel")
	}
}
