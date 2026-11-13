package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// userHZ is the unit of /proc/<pid>/stat's starttime: the kernel exports it in USER_HZ
// ticks, which is 100 on every Linux architecture this image ships (proc_pid_stat(5)).
const userHZ = 100

// lockClockSlack absorbs a small forward wall-clock adjustment between a lock's write and
// the sweep: the kernel recomputes btime from the wall clock, so such a step moves a live
// writer's computed start later, and within this slack it still reads as held.
const lockClockSlack = time.Second

// procView is the process identity facts of one /proc: the kernel boot id and the boot
// instant that /proc/<pid>/stat start ticks count from.
type procView struct {
	btime  time.Time
	root   string
	bootID string
}

func readProcView(root string) (procView, error) {
	id, err := os.ReadFile(filepath.Join(root, "sys", "kernel", "random", "boot_id"))
	if err != nil {
		return procView{}, err
	}
	stat, err := os.ReadFile(filepath.Join(root, "stat"))
	if err != nil {
		return procView{}, err
	}
	sc := bufio.NewScanner(bytes.NewReader(stat))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			secs, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return procView{}, fmt.Errorf("btime %q: %w", v, err)
			}
			return procView{root: root, bootID: strings.TrimSpace(string(id)), btime: time.Unix(secs, 0)}, nil
		}
	}
	return procView{}, errors.New(filepath.Join(root, "stat") + " has no btime line")
}

// startTicks returns pid's start in USER_HZ ticks since boot, and false when no
// process or thread has that id.
func (p procView) startTicks(pid int) (ticks int64, running bool, err error) {
	raw, err := os.ReadFile(filepath.Join(p.root, strconv.Itoa(pid), "stat"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	// Field 2 (comm) is parenthesised and may hold spaces or ')'; starttime is field 22.
	var fields []string
	if _, rest, found := bytes.CutLast(raw, []byte(")")); found {
		fields = strings.Fields(string(rest))
	}
	if len(fields) < 20 {
		return 0, false, fmt.Errorf("process %d: malformed stat", pid)
	}
	ticks, err = strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("process %d: starttime: %w", pid, err)
	}
	if ticks < 0 {
		return 0, false, fmt.Errorf("process %d: negative starttime %d", pid, ticks)
	}
	return ticks, true, nil
}

// The boot id and start ticks shapes kiro-cli accepts as a lock owner's identity; a lock
// with either missing or malformed is judged by its pid and started_at.
var (
	kiroLockBootID = regexp.MustCompile(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
	kiroLockTicks  = regexp.MustCompile(`^\d+$`)
)

// lockHeld reports whether the process a kiro-cli session lock names still runs. Both of
// kiro-cli 3.1.0's lock writers record a positive pid and an RFC 3339 started_at; the V3
// TUI adds its boot id and start ticks. With that identity, same boot and equal start
// ticks decide. Without it, a running pid is held when its process started no later than
// started_at (lockClockSlack aside), since a lock's writer starts before it writes; a later
// start is an error, because btime follows the wall clock and a clock step reads the same
// as a reused pid. A lock missing either required field, or whose process cannot be
// checked, is an error.
func (p procView) lockHeld(raw []byte) (bool, error) {
	var lock struct {
		StartedAt  string `json:"started_at"`
		BootID     string `json:"linux_boot_id"`
		StartTicks string `json:"linux_start_ticks"`
		PID        int    `json:"pid"`
	}
	if err := json.Unmarshal(raw, &lock); err != nil {
		return false, fmt.Errorf("undecodable lock: %w", err)
	}
	if lock.PID <= 0 {
		return false, fmt.Errorf("lock has no positive pid (%d)", lock.PID)
	}
	started, err := time.Parse(time.RFC3339, lock.StartedAt)
	if err != nil {
		return false, fmt.Errorf("lock started_at: %w", err)
	}
	ticks, running, err := p.startTicks(lock.PID)
	if err != nil || !running {
		return false, err
	}
	if kiroLockBootID.MatchString(lock.BootID) && kiroLockTicks.MatchString(lock.StartTicks) {
		return lock.BootID == p.bootID && lock.StartTicks == strconv.FormatInt(ticks, 10), nil
	}
	if start := p.startTime(ticks); start.After(started.Add(lockClockSlack)) {
		return false, fmt.Errorf("process %d reads as started at %s, after its lock's started_at %s: a reused pid or a wall-clock step",
			lock.PID, start.UTC().Format(time.RFC3339), lock.StartedAt)
	}
	return true, nil
}

// startTime splits ticks into whole seconds first: ticks*time.Second overflows
// int64 past about 2.9 years of uptime at USER_HZ 100.
func (p procView) startTime(ticks int64) time.Time {
	return p.btime.Add(time.Duration(ticks/userHZ)*time.Second + time.Duration(ticks%userHZ)*(time.Second/userHZ))
}
