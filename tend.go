package main

// foreman watch: what looks after the watchers, the board open or not. It starts one whose session
// is gone, wakes each when a PR of its changed or its time came, and tells me what the queue would
// show if I were looking: a watcher that waits on me or failed, and one whose skills fell behind
// its repo.

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var (
	// rexWait is how often foreman watch looks for Rex while it isn't answering.
	rexWait = 15 * time.Second
	// skillsCheck is how often it looks whether a watcher's checkout fell behind.
	skillsCheck = time.Hour
)

// notify shows a notification on this Mac. The text travels as an argument, never inside the
// script: a watcher's status can carry what the author of a PR wrote.
var notify = func(title, text string) error {
	_, err := sh("", "osascript",
		"-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
		"-e", "end run", title, text)
	return err
}

// A watchLoop is what foreman watch remembers between polls, so it says each thing once.
type watchLoop struct {
	note    func(text string, c Color)
	lastErr map[string]string    // each watcher's last failure, and the last one starting them
	rexDown string               // why Rex didn't answer, while it doesn't
	alerted map[string]string    // what each watcher waited on when it last told me
	behind  map[string]string    // the upstream commit each watcher was last found behind
	checked map[string]time.Time // when each watcher's checkout was last compared
}

// holdWatchLock takes the lock a foreman watch holds for its life, and fails when another holds
// it: two would each wake a watcher for the same change. The queue reads from the same lock
// whether one runs.
func holdWatchLock() (release func(), err error) {
	file := stateFile + ".watch.lock"
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("foreman watch already runs")
		}
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

// watchRuns is whether a foreman watch holds its lock. A shared lock, so two that ask at once
// don't take each other for it.
func watchRuns() bool {
	f, err := os.Open(stateFile + ".watch.lock")
	if err != nil {
		return false
	}
	defer f.Close()
	return syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) != nil
}

// runWatch is `foreman watch`, which launchd keeps running from login. It prints each start, wake
// and failure with its time, and returns only when the config names no watcher or another foreman
// watch runs.
func runWatch(out io.Writer) error {
	if len(watchers) == 0 {
		return nil
	}
	release, err := holdWatchLock()
	if err != nil {
		return err
	}
	defer release()
	l := &watchLoop{
		note:    func(text string, _ Color) { fmt.Fprintln(out, time.Now().Format("2006-01-02 15:04:05"), text) },
		lastErr: map[string]string{}, alerted: map[string]string{}, behind: map[string]string{}, checked: map[string]time.Time{},
	}
	for {
		time.Sleep(l.tick(time.Now()))
	}
}

// tick is one poll, and returns how long to wait for the next: rexWait while Rex isn't up, as at
// login, else watchPoll.
func (l *watchLoop) tick(now time.Time) time.Duration {
	if _, err := agentSessions(); err != nil {
		if e := firstLine(err.Error()); e != l.rexDown {
			l.note("Waiting for Rex: "+e, cRed)
			l.rexDown = e
		}
		return rexWait
	}
	l.rexDown = ""
	// A watcher whose session is gone (Rex restarted, Claude Code exited) is started again here,
	// unless I stopped it.
	started, err := startWatchers()
	if e := errText(err); e != l.lastErr[""] {
		l.lastErr[""] = e
		if e != "" {
			l.note(e, cRed)
		}
	}
	if len(started) > 0 {
		l.note("Started "+strings.Join(started, ", "), cSubtext)
	}
	sessions, err := agentSessions()
	if err != nil {
		return rexWait
	}
	recorded := loadState().WatcherSessions
	for _, w := range watchers {
		// A watcher that isn't running has the zero session, which waits on nothing.
		s, _ := findWatcher(sessions, w, recorded[w.Name])
		l.alert(w, s)
		l.skills(w, now)
	}
	wakeWatchers(sessions, recorded, l.note, l.lastErr)
	return watchPoll
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return firstLine(err.Error())
}

// alert tells me, once, that the watcher stopped on something only I can settle: a permission
// prompt or a question, where its pass stalls until I answer, or a failure. With the board closed
// nothing else would say so outside Rex.
func (l *watchLoop) alert(w Watcher, s rexSession) {
	what := ""
	switch s.State {
	case "blocked":
		what = w.Name + " waits on you: " + strings.TrimSpace(cmp.Or(s.Kind, "permission")+" "+s.Msg)
	case "error":
		what = strings.TrimSuffix(w.Name+" failed: "+s.Msg, ": ")
	}
	if what == l.alerted[w.Name] {
		return
	}
	l.alerted[w.Name] = what
	if what != "" {
		l.tell(what, cRed)
	}
}

// skills tells me, once per upstream commit, that the watcher's checkout is behind its repo in
// `.claude`: it runs its skills from there, and a skill that moved on changes what a pass does,
// and may change the searches to poll. It only says so. Moving the checkout
// forward stays mine to do.
func (l *watchLoop) skills(w Watcher, now time.Time) {
	if now.Sub(l.checked[w.Name]) < skillsCheck {
		return
	}
	l.checked[w.Name] = now
	base := diffBase(w.Dir, true)
	remote, branch, ok := strings.Cut(base, "/")
	if !ok {
		return // not a checkout with an origin
	}
	// A failed fetch leaves the refs as they were last fetched, by this or by my own fetches.
	_, _ = shFor(fetchTimeout, "", "git", "-C", w.Dir, "fetch", "--quiet", remote, branch)
	tip, err := sh("", "git", "-C", w.Dir, "rev-parse", base)
	if err != nil {
		return
	}
	commits, err := sh("", "git", "-C", w.Dir, "log", "--format=%h", "HEAD.."+base, "--", ".claude")
	if err != nil {
		return
	}
	n := len(strings.Fields(commits))
	if tip = strings.TrimSpace(tip); n == 0 || tip == l.behind[w.Name] {
		return
	}
	l.behind[w.Name] = tip
	l.tell(fmt.Sprintf("%s runs old skills: %d commits on %s changed .claude. Move its checkout forward.", w.Name, n, base), cYellow)
}

// tell notes the line and shows it as a notification.
func (l *watchLoop) tell(text string, c Color) {
	l.note(text, c)
	if err := notify("foreman", text); err != nil {
		l.note("Showing that as a notification: "+firstLine(err.Error()), cRed)
	}
}
