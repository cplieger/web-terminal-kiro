package main

// Tab titles come from kiro-cli's own session record, not terminal.WithInputTitle: rebuilding
// a submitted line from raw bytes fuses text a discard key (Escape, Ctrl-U) dropped onto the
// next prompt. The tab-to-session join is kiro's hook, which inherits WT_TITLE_HANDLE and gets
// session_id on stdin; the KAS log or the process tree can reach the wrong session.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unicode/utf8"

	"github.com/cplieger/atomicfile/v4"
	"github.com/cplieger/runesafe/v3"
	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

const (
	// titleStateDirName holds one hook-written file per tab, named for its title handle.
	titleStateDirName = "session-titles"

	// maxTitleFileBytes bounds a read of a mapping or a session.json; larger is corrupt or hostile.
	maxTitleFileBytes = 64 * 1024

	// placeholderTitle is kiro's "no title yet" value, read as absent.
	placeholderTitle = "New Session"

	// titleStateRoot is not under /config: a mapping means nothing once its tab is gone.
	titleStateRoot = "/tmp/web-terminal-kiro"
)

// sessionNamer is the session manager's client title rung (below a user's pin, above the
// automatic cwd ladder) and its URL alias.
type sessionNamer interface {
	SetSessionTitle(id terminal.SessionID, title string) bool
	// SetSessionAlias names the tab's URL after its kiro-cli session. False for an
	// unknown tab, a value outside the engine's alias grammar, or a session another
	// live tab's alias already carries.
	SetSessionAlias(id terminal.SessionID, alias string) bool
	// List is the live-tab set, the reclaim signal: a closed tab's title is frozen, so
	// syncOne's memo never reaches the SetSessionTitle-false probe again.
	List() []terminal.SessionInfo
}

// pushedTitle is one tab's last push and the kiro session it came from.
type pushedTitle struct {
	kiroID string
	title  string
}

// sessionTitleSync pushes kiro-cli session titles onto the engine's client rung and names
// each tab's URL alias after its kiro session. Live tabs are the engine's and mapped tabs are
// whatever the hook wrote into stateDir; an unmapped tab keeps the automatic name.
type sessionTitleSync struct {
	// Field order satisfies govet fieldalignment; re-run it when adding a field.
	//
	// pushed is each tab's last push, so syncOne can clear a re-pointed tab's old title.
	// pushed, aliased and aliasRefused are poller-goroutine only.
	pushed map[terminal.SessionID]pushedTitle
	// aliased is each tab's applied alias; aliasRefused the refusal last logged, so a
	// refusal logs once and is retried while the tab stays live and mapped.
	aliased      map[terminal.SessionID]string
	aliasRefused map[terminal.SessionID]string
	// mapped is the tab -> kiro-session pairing for readers on other goroutines, recorded
	// at mapping time: a tab with no usable title yet is in mapped and absent from pushed.
	mapped atomic.Pointer[map[terminal.SessionID]string]
	// handleByTab and tabByHandle index one title handle both ways. A tab whose hook never
	// ran leaves both entries behind; sweeping them against pass()'s liveness snapshot would
	// drop a live tab's handle minted before the manager lists the session.
	handleByTab map[terminal.SessionID]string
	tabByHandle map[string]terminal.SessionID
	stateDir    string
	// sessionsRoot is $HOME/.kiro/sessions; findSessionRecord scans its hash level.
	sessionsRoot string
	// mu guards handleByTab and tabByHandle only.
	mu sync.Mutex
}

// newSessionTitleSync builds the syncer over stateRoot and the HOME kiro-cli writes. The
// manager arrives through pass: route wiring needs sessionEnv before the manager exists.
func newSessionTitleSync(stateRoot, home string) *sessionTitleSync {
	if home == "" {
		// An empty HOME makes sessionsRoot relative to the server's cwd.
		slog.Warn("HOME is empty; kiro-cli session titles cannot be resolved and tabs keep the automatic name ladder",
			"hint", "the image sets HOME=/config/home; a compose environment entry that blanks it disables tab titles")
	}
	return &sessionTitleSync{
		stateDir:     filepath.Join(stateRoot, titleStateDirName),
		sessionsRoot: filepath.Join(home, ".kiro", "sessions"),
		pushed:       make(map[terminal.SessionID]pushedTitle),
		aliased:      make(map[terminal.SessionID]string),
		aliasRefused: make(map[terminal.SessionID]string),
		handleByTab:  make(map[terminal.SessionID]string),
		tabByHandle:  make(map[string]terminal.SessionID),
	}
}

// sessionEnv returns the hook's two variables for one tab: the title handle and where to
// write it. The handle stands in for the tab id, which is the /ws attach capability and must
// not name a file in a directory this app does not own.
func (s *sessionTitleSync) sessionEnv(tabID terminal.SessionID) []string {
	handle, err := s.titleHandle(tabID)
	if err != nil {
		slog.Warn("session title: could not mint a title handle; this tab keeps the automatic name ladder",
			"error", err)
		return nil
	}
	return []string{
		// The WT_ prefix namespaces variables injected into the user's shell environment.
		"WT_TITLE_HANDLE=" + handle,
		"WT_TITLE_STATE_DIR=" + s.stateDir,
	}
}

// titleHandle returns this tab's title handle, minting it once: one mapping file per tab.
func (s *sessionTitleSync) titleHandle(tabID terminal.SessionID) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if handle, ok := s.handleByTab[tabID]; ok {
		return handle, nil
	}
	handle, err := newTitleHandle()
	if err != nil {
		return "", err
	}
	s.handleByTab[tabID] = handle
	s.tabByHandle[handle] = tabID
	return handle, nil
}

// handleFor returns tabID's title handle without minting one: no handle means titles are off.
func (s *sessionTitleSync) handleFor(tabID terminal.SessionID) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	handle, ok := s.handleByTab[tabID]
	return handle, ok
}

// seedMapping writes the mapping a kiro-cli hook would write for tabID, so a resumed tab,
// whose kiro-cli fires no hook until the user's next prompt, is titled and mapped from the
// first poller pass. It writes nothing for a tab without a title handle.
func (s *sessionTitleSync) seedMapping(ctx context.Context, tabID terminal.SessionID, kiroID string) error {
	handle, ok := s.handleFor(tabID)
	if !ok {
		return nil
	}
	_, err := atomicfile.WriteFile(ctx, filepath.Join(s.stateDir, handle), []byte(kiroID+"\n"), atomicfile.WithMode(0o600))
	if err != nil {
		slog.Debug("session title: could not seed a restored tab's mapping",
			"title_handle", handle, "error", err)
	}
	return err
}

// tabForHandle resolves a mapping filename to its tab; a handle this process never minted
// names none.
func (s *sessionTitleSync) tabForHandle(handle string) (terminal.SessionID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tabID, ok := s.tabByHandle[handle]
	return tabID, ok
}

// dropHandle forgets one handle in both indexes.
func (s *sessionTitleSync) dropHandle(handle string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tabID, ok := s.tabByHandle[handle]; ok {
		delete(s.handleByTab, tabID)
		delete(s.tabByHandle, handle)
	}
}

// newTitleHandle mints an unguessable 128-bit hex handle that authenticates nothing.
func newTitleHandle() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ensureStateDir creates and verifies the hook's drop directory at start. Not os.MkdirAll:
// titleStateRoot sits in world-writable /tmp, and MkdirAll follows a planted symlink, which
// turns the reclaim sweep into a delete loop (CWE-59, CWE-377).
func (s *sessionTitleSync) ensureStateDir() error {
	for _, dir := range []string{filepath.Dir(s.stateDir), s.stateDir} {
		if err := ensureStateLevel(dir); err != nil {
			return err
		}
	}
	return nil
}

// ensureStateLevel creates one level of the drop directory and verifies it.
func ensureStateLevel(dir string) error {
	created := true
	if err := os.Mkdir(dir, 0o700); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		created = false
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.Mode().IsDir() {
		return fmt.Errorf("%s is not a directory: a symlink or a plain file is planted at that path", dir)
	}
	// Ownership too: another uid's level, even at 0755, can be swapped for a symlink after
	// this check. Both levels, since the root's owner controls the child's name.
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s ownership could not be verified", dir)
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is owned by uid %d, want server uid %d: its owner could replace the checked path", dir, stat.Uid, os.Geteuid())
	}
	perm := fi.Mode().Perm()
	if created && perm != 0o700 {
		// An inheritable group-write ACL widens a fresh mkdir (ZFS nfs4acl: 0770 from 0o700).
		// Only a level this process just created is repaired, never a pre-existing one.
		if perm, err = enforceLevelMode(dir); err != nil {
			return err
		}
	}
	// Write bits only: the handles a listing shows authenticate nothing, so read access
	// discloses no secret; write access would let another user swap the child in.
	if perm&0o022 != 0 {
		return fmt.Errorf("%s is group/other-writable (%#o): another user could replace the mapping files under it", dir, perm)
	}
	return nil
}

// enforceLevelMode sets a just-created level to 0700 through one O_NOFOLLOW descriptor and
// returns the bits the filesystem stored, which ensureStateLevel's write-bits check judges.
func enforceLevelMode(dir string) (os.FileMode, error) {
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	stored, err := atomicfile.EnforceMode(f, 0o700)
	if errors.Is(err, atomicfile.ErrModeNotStored) {
		// Reported, not refused: refusing would turn titles off for good on such a mount.
		return stored.Perm(), nil
	}
	if err != nil {
		return 0, err
	}
	return stored.Perm(), nil
}

// enableSessionTitles verifies the drop directory and returns the per-tab environment, or nil
// when it was refused. Both the env injection and the poller read this one value, so
// "inject but do not poll" cannot be expressed.
func enableSessionTitles(titles *sessionTitleSync) func(tabID terminal.SessionID) []string {
	if err := titles.ensureStateDir(); err != nil {
		slog.Warn("session title state dir refused; tabs keep the automatic name ladder and the title poller stays off",
			"dir", titles.stateDir, "error", err,
			"hint", "kiro-cli session titles need this directory owned by the server uid, not group/other-writable, and writable by the hook it seeds")
		return nil
	}
	return titles.sessionEnv
}

// pass runs one sweep: reclaim every mapping whose tab is gone, then for the ones that
// remain, read that kiro session's title and push it if it changed.
func (s *sessionTitleSync) pass(ctx context.Context, mgr sessionNamer) {
	entries, err := os.ReadDir(s.stateDir)
	if err != nil {
		// No ErrNotExist carve-out: ensureStateDir made it, so absence means something in
		// /tmp removed it, and nothing re-creates it.
		slog.Debug("session title: state dir unreadable; no tab can be mapped until it is back",
			"dir", s.stateDir, "error", err)
		// Publish empty: a stale pairing must not outlive the directory that refreshes it.
		s.mapped.Store(&map[terminal.SessionID]string{})
		return
	}
	// One liveness snapshot, so every mapping is judged against the same picture.
	live := make(map[terminal.SessionID]struct{}, len(entries))
	sessions := mgr.List()
	for i := range sessions {
		live[sessions[i].ID] = struct{}{}
	}
	mapped := 0
	pairs := make(map[terminal.SessionID]string, len(entries))
	// mappingEntries, not len(entries): subdirectories and hook temps are not mappings.
	mappingEntries := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			// The hook's in-flight temp; reclaiming it would drop that prompt's update.
			continue
		}
		mappingEntries++
		tabID, known := s.tabForHandle(e.Name())
		if !known {
			s.forget(e.Name())
			continue
		}
		if _, ok := live[tabID]; !ok {
			delete(s.pushed, tabID)
			s.forget(e.Name())
			continue
		}
		mapped++
		if kiroID := s.syncOne(ctx, mgr, e.Name(), tabID); kiroID != "" {
			pairs[tabID] = kiroID
			s.syncAlias(mgr, tabID, e.Name(), kiroID)
		}
	}
	s.mapped.Store(&pairs)
	s.pruneAliases(live)
	if mapped == 0 && len(live) > 0 {
		// Debug, not Warn: a tab at the device sign-in has no kiro session yet.
		slog.Debug("session title: no live tab has a kiro session mapping; every tab keeps the automatic name ladder",
			"live_tabs", len(live), "state_entries", mappingEntries, "dir", s.stateDir,
			"hint", "kiro-cli's SessionStart/UserPromptSubmit hooks write these files; check $HOME/.kiro/hooks/web-terminal-session-title.json and hooks/session-title.sh. A tab still at the device-flow sign-in has no kiro session yet, so this is expected until chat starts.")
	}
}

// syncOne maps one tab to its kiro session and pushes that session's title. It returns the
// session id it resolved, "" for none, whatever the title outcome.
func (s *sessionTitleSync) syncOne(ctx context.Context, mgr sessionNamer, handle string, tabID terminal.SessionID) string {
	mapped, ok := s.readMapping(ctx, handle)
	if !ok {
		return ""
	}
	kiroID, raw, state := s.tabSession(ctx, mapped)
	previous, pushed := s.pushed[tabID]
	repointed := pushed && previous.kiroID != kiroID
	title := ""
	if state == recordPresent {
		title = titleFromRecord(kiroID, raw)
	}
	if title == "" {
		if !repointed {
			return kiroID
		}
		// Re-pointed to a conversation with no title yet: clear the previous one's title.
		if !mgr.SetSessionTitle(tabID, "") {
			s.forget(handle)
		}
		delete(s.pushed, tabID)
		return kiroID
	}
	if pushed && !repointed && previous.title == title {
		return kiroID
	}
	// False: the tab closed after this sweep's liveness snapshot.
	if !mgr.SetSessionTitle(tabID, title) {
		delete(s.pushed, tabID)
		s.forget(handle)
		return kiroID
	}
	s.pushed[tabID] = pushedTitle{kiroID: kiroID, title: title}
	// The title is the user's first message, so only its length is logged; the tab id is
	// the /ws capability and is never logged, in any form.
	slog.Debug("session title: adopted kiro session title",
		"title_handle", handle, "kiro_session", kiroID,
		"title_runes", utf8.RuneCountInString(title))
	return kiroID
}

// syncAlias names a tab's URL after its kiro session. A refused alias is retried while the
// tab stays live and mapped, so a second tab on one session takes the name once the holder
// closes.
func (s *sessionTitleSync) syncAlias(mgr sessionNamer, tabID terminal.SessionID, handle, kiroID string) {
	if s.aliased[tabID] == kiroID {
		return
	}
	if mgr.SetSessionAlias(tabID, kiroID) {
		s.aliased[tabID] = kiroID
		delete(s.aliasRefused, tabID)
		return
	}
	if s.aliasRefused[tabID] != kiroID {
		s.aliasRefused[tabID] = kiroID
		slog.Debug("session title: URL alias not applied; retried while the tab stays live and mapped",
			"title_handle", handle, "kiro_session", kiroID)
	}
}

func (s *sessionTitleSync) pruneAliases(live map[terminal.SessionID]struct{}) {
	for _, m := range []map[terminal.SessionID]string{s.aliased, s.aliasRefused} {
		maps.DeleteFunc(m, func(id terminal.SessionID, _ string) bool {
			_, ok := live[id]
			return !ok
		})
	}
}

// mappedSessions returns the tab -> kiro-session pairing the last sweep published, nil
// before the first. The map is shared and read-only.
func (s *sessionTitleSync) mappedSessions() map[terminal.SessionID]string {
	if pairs := s.mapped.Load(); pairs != nil {
		return *pairs
	}
	return nil
}

// readMapping reads the session id the hook wrote for handle, validated because it becomes
// a path component.
func (s *sessionTitleSync) readMapping(ctx context.Context, handle string) (string, bool) {
	raw, err := readSmallFile(ctx, filepath.Join(s.stateDir, handle), maxTitleFileBytes)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("session title: mapping file unreadable",
				"title_handle", handle, "error", err)
		}
		return "", false
	}
	id := strings.TrimSpace(string(raw))
	if !validKiroSessionID(id) {
		if id != "" {
			slog.Debug("session title: mapping file holds no usable session id",
				"title_handle", handle)
		}
		return "", false
	}
	return id, true
}

// maxStepOwnerHops bounds tabSession's walk up a chain of step records. KAS refuses a
// workflow launched from a step, so a real chain is one hop; the bound only stops a cycle.
const maxStepOwnerHops = 4

// tabSession returns the conversation a tab holds when its mapping names kiroID, with its
// record. KAS runs a workflow step's session in the launching tab's kiro-cli process, so the
// step's hooks write the step's id over the tab's mapping; a step resolves to its launcher.
func (s *sessionTitleSync) tabSession(ctx context.Context, kiroID string) (id string, raw []byte, state recordState) {
	id = kiroID
	_, raw, state = s.findSessionRecord(ctx, id)
	for range maxStepOwnerHops {
		if state != recordPresent {
			break
		}
		owner := stepOwner(id, raw)
		if owner == "" {
			break
		}
		id = owner
		_, raw, state = s.findSessionRecord(ctx, id)
	}
	return id, raw, state
}

// stepOwner returns the launching session named by kiroID's step record raw, "" when raw is
// not a step record or names no valid owner. Older records lack serverSession and carry only
// rootConversationId, which a top-level session can also carry: only the step marker counts.
func stepOwner(kiroID string, raw []byte) string {
	var rec struct {
		ID                 string `json:"id"`
		RootConversationID string `json:"rootConversationId"`
		Meta               struct {
			Kiro struct {
				Workflow struct {
					Type string `json:"type"`
				} `json:"workflow"`
				ServerSession struct {
					OwnerSessionID string `json:"ownerSessionId"`
				} `json:"serverSession"`
			} `json:"kiro"`
		} `json:"_meta"`
	}
	if json.Unmarshal(raw, &rec) != nil || rec.ID != kiroID || rec.Meta.Kiro.Workflow.Type != "step" {
		return ""
	}
	owner := rec.Meta.Kiro.ServerSession.OwnerSessionID
	if owner == "" {
		owner = rec.RootConversationID
	}
	if !validKiroSessionID(owner) {
		return ""
	}
	return owner
}

// recordState is what findSessionRecord learned about one kiro session's record.
type recordState int

const (
	// recordMissing: the store, or every hash directory, answered ENOENT.
	recordMissing recordState = iota
	// recordPresent: a session.json was read.
	recordPresent
	// recordUnknown: a read failed other than ENOENT, so the record may exist.
	recordUnknown
)

// findSessionRecord scans the per-workspace hash level for kiroID's session.json and
// returns the record's directory and bytes when it read one. With recordUnknown, dir is
// the candidate whose session.json exists but could not be read (oversize, not a regular
// file), or "" when no such candidate was seen.
func (s *sessionTitleSync) findSessionRecord(ctx context.Context, kiroID string) (dir string, raw []byte, state recordState) {
	hashDirs, err := os.ReadDir(s.sessionsRoot)
	if err != nil {
		// A missing tree is normal before kiro-cli's first session.
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil, recordMissing
		}
		slog.Debug("session title: kiro session store unreadable",
			"dir", s.sessionsRoot, "error", err)
		return "", nil, recordUnknown
	}
	state = recordMissing
	for _, hd := range hashDirs {
		if !hd.IsDir() {
			continue
		}
		candidate := filepath.Join(s.sessionsRoot, hd.Name(), kiroID)
		raw, err := readSmallFile(ctx, filepath.Join(candidate, "session.json"), maxTitleFileBytes)
		if err == nil {
			return candidate, raw, recordPresent
		}
		// ENOENT is the normal miss: the session lives under one hash dir.
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("session title: session.json unreadable",
				"kiro_session", kiroID, "error", err)
			state = recordUnknown
			if _, lerr := os.Lstat(filepath.Join(candidate, "session.json")); lerr == nil && dir == "" {
				dir = candidate
			}
		}
	}
	return dir, nil, state
}

// sessionRecord reports whether kiroID's record exists, and its directory when it does or
// when findSessionRecord found it unreadable. A record whose own "id" is not kiroID is not
// this conversation and counts as missing.
func (s *sessionTitleSync) sessionRecord(ctx context.Context, kiroID string) (string, recordState) {
	dir, raw, state := s.findSessionRecord(ctx, kiroID)
	if state != recordPresent {
		return dir, state
	}
	var rec struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil || rec.ID != kiroID {
		return "", recordMissing
	}
	return dir, recordPresent
}

// titleFromRecord extracts the title from one session.json, "" when it has none usable.
func titleFromRecord(kiroID string, raw []byte) string {
	var rec struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		slog.Debug("session title: session.json is not decodable",
			"kiro_session", kiroID, "error", err)
		return ""
	}
	// The engine's rung drops only C0 and DEL, so C1, Bidi and U+2028/29 are sanitized here,
	// before the trim, so a control-only title reads as no title.
	title := strings.TrimSpace(runesafe.SanitizeSingleLine(rec.Title))
	if title == placeholderTitle {
		return ""
	}
	return title
}

// forget removes a dead tab's mapping file and handle. handle must be an os.ReadDir name of
// stateDir, so it is one path component.
func (s *sessionTitleSync) forget(handle string) {
	s.dropHandle(handle)
	if err := os.Remove(filepath.Join(s.stateDir, handle)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Debug("session title: could not drop a stale mapping",
			"title_handle", handle, "error", err)
	}
}

// readSmallFile reads one regular, non-symlink state file of at most limit bytes, refusing a
// larger one. The caller picks the bound: a mapping is a line, a run record holds reports.
func readSmallFile(ctx context.Context, path string, limit int) ([]byte, error) {
	f, _, err := atomicfile.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only
	return atomicfile.ReadBoundedFile(ctx, f, int64(limit))
}

// validKiroResumeID is the only shape passed to --resume-id: "sess_" plus a canonical UUID,
// 41 bytes, within the engine's 64-byte alias bound.
func validKiroResumeID(id string) bool {
	rest, ok := strings.CutPrefix(id, "sess_")
	if !ok || len(rest) != 36 {
		return false
	}
	for i := range len(rest) {
		c := rest[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHexDigit(c) {
				return false
			}
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// validKiroSessionID gates an id from a mapping file or a record before it becomes a path
// component.
func validKiroSessionID(id string) bool {
	rest, ok := strings.CutPrefix(id, "sess_")
	if !ok || rest == "" || len(id) > 128 {
		return false
	}
	for i := range len(rest) {
		if c := rest[i]; c != '-' && !isHexDigit(c) {
			return false
		}
	}
	return true
}
