// Package logs follows container logs of a changing set of pods (stern-like),
// and fetches the previous container's logs when a container restarts.
package logs

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/seneralkan/kubemon/internal/model"
)

// Line is one log line.
type Line struct {
	Time      time.Time
	Pod       string
	Container string
	Text      string
	Previous  bool // from the previous (crashed) container instance
	Err       bool // Text is an error message, not log output
}

// Options configure a Follower.
type Options struct {
	Container string // only this container; "" = all app containers
	// TailLines is the backlog fetched when a stream starts, like kubectl
	// --tail: n > 0 fetches the last n lines, 0 none (only new lines), and a
	// negative value the entire log.
	TailLines int64
}

// backoff holds the retry timings; a field so tests can shorten them.
type backoff struct {
	retryMin, retryMax time.Duration // between in-run reopens of a dropped stream
	quick              time.Duration // a stream ending sooner with no lines is fruitless
	errMin, errMax     time.Duration // Sync waits this long after a failed open
}

var defaultBackoff = backoff{
	retryMin: time.Second, retryMax: 30 * time.Second,
	quick:  time.Second,
	errMin: 10 * time.Second, errMax: 60 * time.Second,
}

// maxFruitless consecutive fruitless reopens mean the container has exited
// (its finished log returns EOF at once); run then hands back to Sync.
const maxFruitless = 3

// openFunc opens a log stream of one pod.
type openFunc func(ctx context.Context, pod string, o *corev1.PodLogOptions) (io.ReadCloser, error)

// Follower streams logs for the pods given to Sync.
type Follower struct {
	opts Options
	open openFunc
	bo   backoff
	out  chan Line
	wg   sync.WaitGroup // stream goroutines

	mu        sync.Mutex
	ctx       context.Context // from the first Sync; parent of every stream
	closed    bool            // ctx is done: no new goroutines may start
	forbidden bool            // pods/log is forbidden: open no more streams
	pods      map[string]*podState
}

type podState struct {
	ctx        context.Context // canceled when the pod leaves the followed set
	cancel     context.CancelFunc
	containers map[string]*containerState
}

type containerState struct {
	streaming bool
	followed  bool      // a follow stream has run before
	restarts  int32     // RestartCount of the instance last followed
	last      time.Time // timestamp of the last line received from it
	prevSeen  int32     // highest RestartCount whose previous logs were fetched

	errMsg     string        // last open error reported as a Line
	errBackoff time.Duration // current wait after a failed open; 0 after a good line
	retryAt    time.Time     // Sync opens no follow stream before this
}

// NewFollower creates a follower for one namespace.
func NewFollower(c kubernetes.Interface, namespace string, o Options) *Follower {
	open := func(ctx context.Context, pod string, lo *corev1.PodLogOptions) (io.ReadCloser, error) {
		return c.CoreV1().Pods(namespace).GetLogs(pod, lo).Stream(ctx)
	}
	return newFollower(open, o)
}

func newFollower(open openFunc, o Options) *Follower {
	return &Follower{opts: o, open: open, bo: defaultBackoff, out: make(chan Line, 1024), pods: map[string]*podState{}}
}

// Lines is the merged output of all streams. Closed when the Sync context ends
// and all streams have stopped. It never closes if Sync is never called.
func (f *Follower) Lines() <-chan Line { return f.out }

// Sync makes the followed set equal to pods: starts streams for running
// containers not yet followed, stops streams of pods no longer listed, and
// emits previous-container logs once per new restart. Safe to call often.
// Streams live under the ctx of the first call; later contexts are ignored.
//
// A failed open is emitted once as an Err line and not retried before a
// backoff (10s doubling to 60s). If pods/log is forbidden, that is emitted
// once and no further streams are opened.
func (f *Follower) Sync(ctx context.Context, pods []model.Pod) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ctx == nil {
		f.ctx = ctx
		go f.closeWhenDone(ctx)
	}
	if f.closed {
		return
	}

	listed := make(map[string]bool, len(pods))
	for _, p := range pods {
		listed[p.Name] = true
		ps := f.pods[p.Name]
		if ps == nil {
			pctx, cancel := context.WithCancel(f.ctx)
			ps = &podState{ctx: pctx, cancel: cancel, containers: map[string]*containerState{}}
			f.pods[p.Name] = ps
		}
		for _, c := range p.Containers {
			if c.Init || (f.opts.Container != "" && c.Name != f.opts.Container) {
				continue
			}
			cs := ps.containers[c.Name]
			if cs == nil {
				cs = &containerState{}
				ps.containers[c.Name] = cs
			}
			f.syncContainer(ps.ctx, p.Name, c, cs)
		}
	}
	for name, ps := range f.pods {
		if !listed[name] {
			ps.cancel()
			delete(f.pods, name)
		}
	}
}

// syncContainer starts whatever one container needs. f.mu must be held.
func (f *Follower) syncContainer(ctx context.Context, pod string, c model.Container, cs *containerState) {
	if f.forbidden {
		return
	}
	prev := c.RestartCount > cs.prevSeen && c.Last != nil
	if prev {
		cs.prevSeen = c.RestartCount
	}

	var follow *corev1.PodLogOptions
	var after time.Time
	if c.State == "Running" && !cs.streaming && !time.Now().Before(cs.retryAt) {
		follow = &corev1.PodLogOptions{Container: c.Name, Follow: true, Timestamps: true}
		if cs.followed && cs.restarts == c.RestartCount && !cs.last.IsZero() {
			// Same instance, stream dropped: resume where it stopped.
			since := metav1.NewTime(cs.last)
			follow.SinceTime = &since
			after = cs.last
		} else {
			follow.TailLines = f.tail()
		}
		cs.streaming, cs.followed, cs.restarts = true, true, c.RestartCount
	}

	if !prev && follow == nil {
		return
	}
	f.wg.Add(1)
	go f.run(ctx, pod, c.Name, cs, prev, follow, after)
}

// run emits the previous instance's logs (so they come first), then follows
// the current one. When the follow stream ends while ctx is alive (a dropped
// connection), run reopens it itself after a backoff (1s doubling to 30s),
// resuming after the last line, so a quiet namespace without further Syncs
// keeps streaming. cs.streaming stays true meanwhile, so Sync does not start
// a second stream. run gives up, handing back to Sync, on an open error that
// is not transient, or after maxFruitless reopens in a row that ended
// quickly without a line (the container exited).
func (f *Follower) run(ctx context.Context, pod, container string, cs *containerState, prev bool, follow *corev1.PodLogOptions, after time.Time) {
	defer f.wg.Done()
	if prev {
		o := &corev1.PodLogOptions{Container: container, Previous: true, Timestamps: true, TailLines: f.tail()}
		if _, n, err := f.copy(ctx, pod, o, time.Time{}); err != nil {
			f.reportErr(ctx, pod, container, cs, err)
		} else if n > 0 {
			f.clearErr(cs)
		}
	}
	if follow == nil {
		return
	}

	last := after
	delay := f.bo.retryMin
	var err error
	for fruitless := 0; ; {
		start := time.Now()
		var n int
		last, n, err = f.copy(ctx, pod, follow, last)
		if ctx.Err() != nil {
			err = nil // canceled, not failed
			break
		}
		if err != nil {
			f.reportErr(ctx, pod, container, cs, err)
		}
		if n > 0 {
			f.clearErr(cs)
		}
		if n > 0 || (err == nil && time.Since(start) >= f.bo.quick) {
			fruitless, delay = 0, f.bo.retryMin
		} else {
			fruitless++
		}
		if (err != nil && !transient(err)) || fruitless >= maxFruitless || f.isForbidden() {
			break
		}

		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
		}
		if ctx.Err() != nil {
			err = nil
			break
		}
		delay = min(2*delay, f.bo.retryMax)
		if !last.IsZero() {
			o := *follow
			since := metav1.NewTime(last)
			o.SinceTime, o.TailLines = &since, nil
			follow = &o
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	cs.streaming = false
	cs.last = last
	if err != nil { // the last open failed: make Sync wait before retrying
		cs.errBackoff = min(max(2*cs.errBackoff, f.bo.errMin), f.bo.errMax)
		cs.retryAt = time.Now().Add(cs.errBackoff)
	}
}

// transient reports whether an open error may go away on its own soon:
// network errors and server-side timeouts, throttling or 5xx.
func transient(err error) bool {
	var st apierrors.APIStatus
	if !errors.As(err, &st) {
		return true
	}
	return apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsInternalError(err) || apierrors.IsServiceUnavailable(err) || apierrors.IsUnexpectedServerError(err)
}

func (f *Follower) isForbidden() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.forbidden
}

// reportErr emits an open error as an Err line, once per distinct message
// per container; a Forbidden error once per follower, which also stops new
// streams.
func (f *Follower) reportErr(ctx context.Context, pod, container string, cs *containerState, err error) {
	f.mu.Lock()
	var emit bool
	if apierrors.IsForbidden(err) {
		emit, f.forbidden = !f.forbidden, true
	} else if msg := err.Error(); cs.errMsg != msg {
		emit, cs.errMsg = true, msg
	}
	f.mu.Unlock()
	if !emit {
		return
	}
	l := Line{Time: time.Now(), Pod: pod, Container: container, Text: "logs unavailable: " + err.Error(), Err: true}
	select {
	case f.out <- l:
	case <-ctx.Done():
	}
}

// clearErr forgets the error state of a container that produced a line.
func (f *Follower) clearErr(cs *containerState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cs.errMsg, cs.errBackoff = "", 0
}

// copy sends the lines of one log request to f.out, skipping lines timestamped
// at or before after. It returns the timestamp of the last line seen (or
// after), the number of lines sent, and the open error if any. A read error
// just ends the stream, like EOF.
func (f *Follower) copy(ctx context.Context, pod string, o *corev1.PodLogOptions, after time.Time) (last time.Time, n int, err error) {
	last = after
	rc, err := f.open(ctx, pod, o)
	if err != nil {
		return last, 0, err
	}
	defer rc.Close()
	stop := context.AfterFunc(ctx, func() { rc.Close() }) // unblock a pending Read
	defer stop()

	r := bufio.NewReader(rc)
	for {
		s, rerr := r.ReadString('\n')
		if s != "" {
			t, text, ok := parseLine(s)
			if !ok || t.After(after) {
				l := Line{Time: t, Pod: pod, Container: o.Container, Text: text, Previous: o.Previous}
				select {
				case f.out <- l:
					n++
				case <-ctx.Done():
					return last, n, nil
				}
				if ok {
					last = t
				}
			}
		}
		if rerr != nil {
			return last, n, nil
		}
	}
}

// parseLine splits the RFC3339Nano timestamp prefix added by Timestamps: true.
// ok is false when there is none; the time is then now and text the whole line.
func parseLine(s string) (t time.Time, text string, ok bool) {
	s = strings.TrimRight(s, "\r\n")
	ts, text, _ := strings.Cut(s, " ")
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t, text, true
	}
	return time.Now(), s, false
}

// tail is the TailLines option; nil (everything) when negative.
func (f *Follower) tail() *int64 {
	if f.opts.TailLines < 0 {
		return nil
	}
	n := f.opts.TailLines
	return &n
}

func (f *Follower) closeWhenDone(ctx context.Context) {
	<-ctx.Done()
	f.mu.Lock()
	f.closed = true // after this no wg.Add can race with Wait
	f.mu.Unlock()
	f.wg.Wait()
	close(f.out)
}
