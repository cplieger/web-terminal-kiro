package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The locks under testdata/kirolocks were written by kiro-cli 3.1.0 on one host: v3-tui*
// by the TUI (chat --v3, and chat --resume-id in a running container), v2-engine* by the
// V2 ACP engine (acp --agent-engine=v2, new and resumed), settings-cli.json.lock by its
// settings writer. The pids and start ticks below were read from that host's /proc while
// each writer ran.
const (
	capturedBootID = "5a48e6ee-ebd0-4ef3-b2e6-734ba9929033"
	otherBootID    = "0b1c2d3e-4f50-4617-8293-a4b5c6d7e8f9"
)

var capturedBtime = time.Unix(1793963849, 0)

type capturedLock struct {
	file  string
	pid   int
	ticks int64
}

var capturedSessionLocks = []capturedLock{
	{file: "v3-tui.lock", pid: 287329, ticks: 12094754},
	{file: "v3-tui-resume.lock", pid: 1551, ticks: 11509999},
	{file: "v2-engine.lock", pid: 294373, ticks: 12104304},
	{file: "v2-engine-resume.lock", pid: 296129, ticks: 12106790},
}

// oneHourOfTicks is an hour in /proc's USER_HZ (100) ticks.
const oneHourOfTicks = 360000

func readCaptured(t *testing.T, file string) string {
	t.Helper()
	return string(mustReadFile(t, filepath.Join("testdata", "kirolocks", file)))
}

// exitedPID runs this test binary to completion and returns its pid, which no process holds
// once the child is reaped.
func exitedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run a child to free its pid: %v", err)
	}
	return cmd.Process.Pid
}

// plantStaleLock writes the lock a stopped kiro-cli leaves, in the V2 engine's
// {"pid","started_at"} shape naming a pid no process holds, into dir and returns its path.
func plantStaleLock(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, ".lock")
	body := `{"pid":` + strconv.Itoa(exitedPID(t)) + `,"started_at":"` + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano) + `"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("plant .lock: %v", err)
	}
	return path
}

// resumeC is a third well-formed session id beside tabstate_test.go's two.
const resumeC = "sess_47400f57-1c2d-4e5f-8a9b-00000000000c"

func plantFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return err == nil
}

// fakeProc writes a /proc tree holding bootID, btime and one stat file per pid in stats,
// and returns its root.
func fakeProc(t *testing.T, bootID string, btime time.Time, stats map[int]string) string {
	t.Helper()
	root := t.TempDir()
	plantFile(t, filepath.Join(root, "sys", "kernel", "random", "boot_id"), bootID+"\n")
	plantFile(t, filepath.Join(root, "stat"), "cpu  1 2 3 4\nbtime "+strconv.FormatInt(btime.Unix(), 10)+"\nprocesses 9\n")
	for pid, stat := range stats {
		plantFile(t, filepath.Join(root, strconv.Itoa(pid), "stat"), stat)
	}
	return root
}

// statLine is a /proc/<pid>/stat line whose comm holds a space and a ')', starting at ticks.
func statLine(pid int, ticks int64) string {
	return strconv.Itoa(pid) + " (kiro cli) x) S 1 1 1 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 " +
		strconv.FormatInt(ticks, 10) + " 4595712 582\n"
}

// viewOf is a /proc of bootID and btime in which each pid in running started at its ticks,
// and no other pid runs.
func viewOf(t *testing.T, bootID string, btime time.Time, running map[int]int64) procView {
	t.Helper()
	stats := make(map[int]string, len(running))
	for pid, ticks := range running {
		stats[pid] = statLine(pid, ticks)
	}
	view, err := readProcView(fakeProc(t, bootID, btime, stats))
	if err != nil {
		t.Fatal(err)
	}
	return view
}

// capturedView is the capture host's /proc with the writers of the named captured locks
// still running.
func capturedView(t *testing.T, writers ...capturedLock) procView {
	t.Helper()
	running := make(map[int]int64, len(writers))
	for _, w := range writers {
		running[w.pid] = w.ticks
	}
	return viewOf(t, capturedBootID, capturedBtime, running)
}

// verdict is what clearStaleKiroLock should do with one lock.
type verdict int

const (
	lockKept     verdict = iota // kept, no error
	lockRemoved                 // removed, no error
	lockDoubtful                // kept, with the reason as an error
)

func (v verdict) String() string {
	return [...]string{"kept", "removed", "kept with a reason"}[v]
}

func judged(gotRemoved bool, err error) verdict {
	switch {
	case err != nil:
		return lockDoubtful
	case gotRemoved:
		return lockRemoved
	}
	return lockKept
}

// clockStep is a forward wall-clock step between a lock's write and the sweep, larger than
// either captured V2 writer's lead over its started_at (16.0 s and 19.8 s) plus the slack.
const clockStep = 30 * time.Second

func TestClearStaleKiroLockJudgesCapturedLocks(t *testing.T) {
	type judgeCase struct {
		view procView
		name string
		v3   verdict
		v2   verdict
	}
	for _, c := range capturedSessionLocks {
		cases := []judgeCase{
			{name: "while its writer runs", view: capturedView(t, c), v3: lockKept, v2: lockKept},
			{name: "once its pid is free", view: capturedView(t), v3: lockRemoved, v2: lockRemoved},
			{
				// Without start ticks, a later start reads the same as a clock step.
				name: "when a process started later holds its pid",
				view: viewOf(t, capturedBootID, capturedBtime, map[int]int64{c.pid: c.ticks + oneHourOfTicks}),
				v3:   lockRemoved, v2: lockDoubtful,
			},
			{
				name: "when a process of a later boot holds its pid",
				view: viewOf(t, otherBootID, capturedBtime.Add(24*time.Hour), map[int]int64{c.pid: c.ticks}),
				v3:   lockRemoved, v2: lockDoubtful,
			},
			{
				name: "while its writer runs after a forward clock step",
				view: viewOf(t, capturedBootID, capturedBtime.Add(clockStep), map[int]int64{c.pid: c.ticks}),
				v3:   lockKept, v2: lockDoubtful,
			},
			{
				// Only the TUI's start-ticks identity tells its writer from a process that
				// took the pid before the lock was written.
				name: "when a process started earlier holds its pid",
				view: viewOf(t, capturedBootID, capturedBtime, map[int]int64{c.pid: c.ticks - 100}),
				v3:   lockRemoved, v2: lockKept,
			},
		}
		for _, tt := range cases {
			want := tt.v2
			if strings.HasPrefix(c.file, "v3-") {
				want = tt.v3
			}
			t.Run(c.file+" "+tt.name, func(t *testing.T) {
				lock := plantFile(t, filepath.Join(t.TempDir(), ".lock"), readCaptured(t, c.file))
				gotRemoved, err := clearStaleKiroLock(t.Context(), lock, tt.view)
				if got := judged(gotRemoved, err); got != want {
					t.Errorf("clearStaleKiroLock = (%v, %v), a %v lock, want %v", gotRemoved, err, got, want)
				}
				if kept := exists(t, lock); kept != (want != lockRemoved) {
					t.Errorf("lock kept = %v, want %v", kept, want != lockRemoved)
				}
			})
		}
	}
}

// A lock with no owner identity (every V2 lock) is judged by its started_at against a start
// computed from btime, which the kernel recomputes from the wall clock; a forward
// adjustment within lockClockSlack still reads as held.
func TestClearStaleKiroLockAllowsClockAdjustment(t *testing.T) {
	for _, c := range capturedSessionLocks[2:] {
		t.Run(c.file, func(t *testing.T) {
			body := readCaptured(t, c.file)
			var written struct {
				StartedAt time.Time `json:"started_at"`
			}
			if err := json.Unmarshal([]byte(body), &written); err != nil {
				t.Fatal(err)
			}
			halfSecondLate := int64(written.StartedAt.Sub(capturedBtime)/(10*time.Millisecond)) + 50
			lock := plantFile(t, filepath.Join(t.TempDir(), ".lock"), body)
			view := viewOf(t, capturedBootID, capturedBtime, map[int]int64{c.pid: halfSecondLate})
			if removed, err := clearStaleKiroLock(t.Context(), lock, view); removed || err != nil {
				t.Errorf("clearStaleKiroLock = (%v, %v), want the lock kept", removed, err)
			}
		})
	}
}

// kiro-cli accepts an owner identity only in its own shapes; any other falls back to the
// pid and started_at.
func TestClearStaleKiroLockIgnoresAnIdentityKiroCLIRejects(t *testing.T) {
	c := capturedSessionLocks[0]
	body := strings.Replace(readCaptured(t, c.file), capturedBootID, "not-a-boot-id", 1)
	lock := plantFile(t, filepath.Join(t.TempDir(), ".lock"), body)
	if removed, err := clearStaleKiroLock(t.Context(), lock, capturedView(t, c)); removed || err != nil {
		t.Errorf("clearStaleKiroLock = (%v, %v), want the lock of a running writer kept", removed, err)
	}
}

func TestClearStaleKiroLockKeepsWhatItCannotJudge(t *testing.T) {
	v2 := capturedSessionLocks[2]
	settingsWriter := capturedLock{pid: 279357, ticks: 12000000}
	body := func(raw string) func(t *testing.T, path string) {
		return func(t *testing.T, path string) { plantFile(t, path, raw) }
	}
	tests := []struct {
		plant func(t *testing.T, path string)
		name  string
	}{
		{name: "kiro-cli's settings lock, which has no started_at", plant: func(t *testing.T, path string) {
			plantFile(t, path, readCaptured(t, "settings-cli.json.lock"))
		}},
		{name: "a pid with no started_at", plant: body(`{"pid":294373}`)},
		{name: "an unparseable started_at", plant: body(`{"pid":294373,"started_at":"yesterday"}`)},
		{name: "a body that is not JSON", plant: body("not json")},
		{name: "no pid", plant: body(`{"started_at":"2026-11-07T20:55:08.089496201Z"}`)},
		{name: "a pid that is not positive", plant: body(`{"pid":0,"started_at":"2026-11-07T20:55:08.089496201Z"}`)},
		{name: "a lock over the read bound", plant: func(t *testing.T, path string) {
			plantFile(t, path, readCaptured(t, v2.file)+strings.Repeat(" ", maxKiroLockBytes))
		}},
		{name: "a symlink", plant: func(t *testing.T, path string) {
			target := plantFile(t, filepath.Join(t.TempDir(), "target"), readCaptured(t, v2.file))
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a directory", plant: func(t *testing.T, path string) {
			if err := os.MkdirAll(filepath.Join(path, "inner"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lock := filepath.Join(t.TempDir(), ".lock")
			tt.plant(t, lock)
			removed, err := clearStaleKiroLock(t.Context(), lock, capturedView(t, v2, settingsWriter))
			if removed || err == nil {
				t.Errorf("clearStaleKiroLock = (%v, %v), want the lock kept and a reason", removed, err)
			}
			if !exists(t, lock) {
				t.Error("a lock that could not be judged was removed")
			}
		})
	}

	t.Run("a writer whose stat cannot be parsed", func(t *testing.T) {
		view, err := readProcView(fakeProc(t, capturedBootID, capturedBtime, map[int]string{v2.pid: "garbage with no comm"}))
		if err != nil {
			t.Fatal(err)
		}
		lock := plantFile(t, filepath.Join(t.TempDir(), ".lock"), readCaptured(t, v2.file))
		if removed, err := clearStaleKiroLock(t.Context(), lock, view); removed || err == nil {
			t.Errorf("clearStaleKiroLock = (%v, %v), want the lock kept and a reason", removed, err)
		}
		if !exists(t, lock) {
			t.Error("a lock whose process could not be checked was removed")
		}
	})

	t.Run("no lock", func(t *testing.T) {
		if removed, err := clearStaleKiroLock(t.Context(), filepath.Join(t.TempDir(), ".lock"), capturedView(t)); removed || err != nil {
			t.Errorf("clearStaleKiroLock = (%v, %v), want (false, nil)", removed, err)
		}
	})
}

// ownStartTicks reads this process's starttime (field 22) from the kernel's own /proc.
func ownStartTicks(t *testing.T) string {
	t.Helper()
	raw := string(mustReadFile(t, "/proc/self/stat"))
	fields := strings.Fields(raw[strings.LastIndexByte(raw, ')')+1:])
	return fields[19]
}

func TestSweepStaleKiroLocksReadsTheRunningKernel(t *testing.T) {
	logs := capturedLog(t)
	me := strconv.Itoa(os.Getpid())
	bootID := strings.TrimSpace(string(mustReadFile(t, "/proc/sys/kernel/random/boot_id")))
	v3, v2 := capturedSessionLocks[0], capturedSessionLocks[2]
	asPID := func(c capturedLock, pid string, more ...string) string {
		pairs := append([]string{`"pid":` + strconv.Itoa(c.pid), `"pid":` + pid}, more...)
		return strings.NewReplacer(pairs...).Replace(readCaptured(t, c.file))
	}
	v2Started := `"started_at":"2026-11-07T20:55:08.089496201Z"`
	startedAt := func(at time.Time) string { return `"started_at":"` + at.UTC().Format(time.RFC3339Nano) + `"` }
	root := filepath.Join(t.TempDir(), "sessions")
	// A kiro-cli started by docker exec while the entrypoint ran: its locks predate the server.
	liveV3 := plantFile(t, filepath.Join(root, "hash0", resumeA, ".lock"), asPID(v3, me,
		capturedBootID, bootID, `"linux_start_ticks":"`+strconv.FormatInt(v3.ticks, 10)+`"`, `"linux_start_ticks":"`+ownStartTicks(t)+`"`))
	liveV2 := plantFile(t, filepath.Join(root, "cli", "47400f57-1c2d-4e5f-8a9b-00000000000e.lock"), asPID(v2, me,
		v2Started, startedAt(time.Now())))
	olderThanItsPID := plantFile(t, filepath.Join(root, "cli", "47400f57-1c2d-4e5f-8a9b-00000000000f.lock"), asPID(v2, me,
		v2Started, startedAt(time.Now().Add(-time.Hour))))
	stale := plantFile(t, filepath.Join(root, "cli", "47400f57-1c2d-4e5f-8a9b-00000000000d.lock"), asPID(v2, strconv.Itoa(exitedPID(t))))

	sweepStaleKiroLocks(t.Context(), root)

	for _, p := range []string{liveV3, liveV2, olderThanItsPID} {
		if !exists(t, p) {
			t.Errorf("the lock of a running process was removed: %s", p)
		}
	}
	if exists(t, stale) {
		t.Error("the lock of a pid no process holds survived")
	}
	if got := logs.String(); strings.Count(got, "level=WARN") != 1 || !strings.Contains(got, olderThanItsPID) || !strings.Contains(got, "cleared=1") {
		t.Errorf("want one Warn naming %s and cleared=1, got log %q", olderThanItsPID, got)
	}
}

func TestReadProcView(t *testing.T) {
	t.Run("reads the boot id and btime", func(t *testing.T) {
		view, err := readProcView(fakeProc(t, capturedBootID, capturedBtime, nil))
		if err != nil {
			t.Fatal(err)
		}
		if view.bootID != capturedBootID || !view.btime.Equal(capturedBtime) {
			t.Errorf("readProcView = (%q, %v), want (%q, %v)", view.bootID, view.btime, capturedBootID, capturedBtime)
		}
	})
	for name, stat := range map[string]string{"no btime line": "cpu 1 2\n", "a malformed btime": "btime soon\n"} {
		t.Run(name, func(t *testing.T) {
			root := fakeProc(t, capturedBootID, capturedBtime, nil)
			plantFile(t, filepath.Join(root, "stat"), stat)
			if _, err := readProcView(root); err == nil {
				t.Error("readProcView accepted it")
			}
		})
	}
	t.Run("no boot id", func(t *testing.T) {
		root := fakeProc(t, capturedBootID, capturedBtime, nil)
		if err := os.Remove(filepath.Join(root, "sys", "kernel", "random", "boot_id")); err != nil {
			t.Fatal(err)
		}
		if _, err := readProcView(root); err == nil {
			t.Error("readProcView accepted a /proc with no boot id")
		}
	})
}

// plantCapturedStore lays out a session store the way kiro-cli 3.1.0 does, holding each
// captured session lock at the path its writer gave it, and returns the lock paths by file.
func plantCapturedStore(t *testing.T, root string) map[string]string {
	t.Helper()
	paths := map[string]string{
		"v3-tui.lock":           filepath.Join(root, "9ae94f6048039017", "sess_a46ab33c-59c1-410c-8f69-95a7c58c4de3", ".lock"),
		"v3-tui-resume.lock":    filepath.Join(root, "c52ddf65534b7b46", "sess_1d2d92db-02cd-49e2-8df6-35ff58ce6420", ".lock"),
		"v2-engine.lock":        filepath.Join(root, "cli", "f1990a4c-18cc-48b7-82ca-3faf7ae61bf2.lock"),
		"v2-engine-resume.lock": filepath.Join(root, "cli", "9689ced3-34bc-4746-8b2b-6ff67bb3d939.lock"),
	}
	for file, path := range paths {
		plantFile(t, path, readCaptured(t, file))
	}
	return paths
}

func TestSweepStaleKiroLocks(t *testing.T) {
	t.Run("clears the stale locks of both layouts and nothing else", func(t *testing.T) {
		logs := capturedLog(t)
		root := filepath.Join(t.TempDir(), "sessions")
		locks := plantCapturedStore(t, root)
		others := []string{
			plantFile(t, filepath.Join(root, "9ae94f6048039017", ".index", ".lock"), ""),
			plantFile(t, filepath.Join(root, "cli", ".index", ".lock"), ""),
			plantFile(t, filepath.Join(root, "cli", "f1990a4c-18cc-48b7-82ca-3faf7ae61bf2.json"), "{}"),
			plantFile(t, filepath.Join(root, "cli", "47400f57-1c2d-4e5f-8a9b-00000000000a", "task.json"), "{}"),
			plantFile(t, filepath.Join(root, "cli", "index.json.lock", "owner.json"), "{}"),
			plantFile(t, filepath.Join(root, "9ae94f6048039017", "sess_-", ".lock"), "not json"),
			plantFile(t, filepath.Join(root, "9ae94f6048039017", resumeC), "a file, not a session"),
			plantFile(t, filepath.Join(root, "stray-file"), ""),
		}

		sweepKiroLocks(t.Context(), root, capturedView(t, capturedSessionLocks[0], capturedSessionLocks[2]))

		for _, f := range []string{"v3-tui-resume.lock", "v2-engine-resume.lock"} {
			if exists(t, locks[f]) {
				t.Errorf("the stale lock %s survived the sweep", locks[f])
			}
		}
		for _, p := range append(others, locks["v3-tui.lock"], locks["v2-engine.lock"]) {
			if !exists(t, p) {
				t.Errorf("%s was removed, want it kept", p)
			}
		}
		if got := logs.String(); !strings.Contains(got, "cleared=2") || strings.Contains(got, "level=WARN") {
			t.Errorf("want one summary naming cleared=2 and no warning, got log %q", got)
		}
	})

	t.Run("a lock it cannot judge is kept and warned with its path", func(t *testing.T) {
		logs := capturedLog(t)
		root := filepath.Join(t.TempDir(), "sessions")
		settingsShaped := plantFile(t, filepath.Join(root, "hash0", resumeA, ".lock"), readCaptured(t, "settings-cli.json.lock"))
		notJSON := plantFile(t, filepath.Join(root, "cli", "47400f57-1c2d-4e5f-8a9b-00000000000e.lock"), "not json")

		sweepKiroLocks(t.Context(), root, capturedView(t, capturedLock{pid: 279357, ticks: 12000000}))

		got := logs.String()
		for _, p := range []string{settingsShaped, notJSON} {
			if !exists(t, p) {
				t.Errorf("%s was removed, want it kept", p)
			}
			if !strings.Contains(got, p) {
				t.Errorf("no warning names %s, got log %q", p, got)
			}
		}
		const msg = "kept a lock that could not be judged or removed"
		if n := strings.Count(got, msg); n != 2 || strings.Contains(got, "cleared=") {
			t.Errorf("want two %q records and no cleared summary, got log %q", msg, got)
		}
	})

	t.Run("an unreadable store warns", func(t *testing.T) {
		logs := capturedLog(t)
		root := plantFile(t, filepath.Join(t.TempDir(), "sessions"), "not a dir")
		sweepKiroLocks(t.Context(), root, capturedView(t))
		if !strings.Contains(logs.String(), "the session store is unreadable") {
			t.Errorf("want the unreadable-store Warn, got log %q", logs.String())
		}
	})

	t.Run("an unreadable /proc warns and clears nothing", func(t *testing.T) {
		logs := capturedLog(t)
		root := filepath.Join(t.TempDir(), "sessions")
		locks := plantCapturedStore(t, root)
		prev := procRoot
		procRoot = filepath.Join(t.TempDir(), "absent")
		t.Cleanup(func() { procRoot = prev })

		sweepStaleKiroLocks(t.Context(), root)

		for _, p := range locks {
			if !exists(t, p) {
				t.Errorf("a sweep that could not read /proc removed %s", p)
			}
		}
		if !strings.Contains(logs.String(), "/proc is unreadable") {
			t.Errorf("want the unreadable-/proc Warn, got log %q", logs.String())
		}
	})

	t.Run("a missing store and a relative root are silent no-ops", func(t *testing.T) {
		logs := capturedLog(t)
		sweepStaleKiroLocks(t.Context(), filepath.Join(t.TempDir(), "absent"))
		t.Chdir(t.TempDir())
		lock := plantStaleLock(t, filepath.Dir(plantFile(t, filepath.Join(".kiro", "sessions", "hash0", resumeA, "session.json"), "{}")))
		sweepStaleKiroLocks(t.Context(), filepath.Join(".kiro", "sessions"))
		if !exists(t, lock) {
			t.Error("a relative root was swept")
		}
		if logs.Len() != 0 {
			t.Errorf("want no log, got %q", logs.String())
		}
	})

	t.Run("a cancelled context stops the sweep", func(t *testing.T) {
		root := t.TempDir()
		locks := plantCapturedStore(t, root)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		sweepKiroLocks(ctx, root, capturedView(t))
		for _, p := range locks {
			if !exists(t, p) {
				t.Errorf("a cancelled sweep removed %s", p)
			}
		}
	})
}

// TestProcStartTimeAfterYearsOfUptime: a process started four years and 0.37 s
// after boot, past the uptime where ticks*time.Second overflows int64.
func TestProcStartTimeAfterYearsOfUptime(t *testing.T) {
	boot := time.Unix(1_600_000_000, 0)
	view := procView{btime: boot}
	const secs = 4 * 365 * 24 * 3600
	got := view.startTime(secs*userHZ + 37)
	if want := boot.Add(secs*time.Second + 370*time.Millisecond); !got.Equal(want) {
		t.Errorf("startTime = %v, want %v", got, want)
	}
}
