package server

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ActivityFile records what each device has been doing. The server owns
// it; the `dropgitbox device` commands only read it.
const ActivityFile = "activity.json"

// Sync states a client reports for a folder.
const (
	StateOK       = "ok"       // synced successfully
	StateConflict = "conflict" // paused on a merge conflict
	StateError    = "error"    // last sync failed (network, disk, ...)
)

// DeviceActivity is what the server knows about one device.
type DeviceActivity struct {
	LastSeen      time.Time                `json:"last_seen"`
	LastIP        string                   `json:"last_ip,omitempty"`
	ClientVersion string                   `json:"client_version,omitempty"`
	Repos         map[string]*RepoActivity `json:"repos,omitempty"`
}

// RepoActivity is one device's activity for one repository.
type RepoActivity struct {
	LastFetch time.Time `json:"last_fetch,omitzero"`
	LastPush  time.Time `json:"last_push,omitzero"`
	PushHead  string    `json:"push_head,omitempty"`

	// Reported by the client after each sync attempt.
	State     string    `json:"state,omitempty"`
	Message   string    `json:"message,omitempty"`
	Conflicts []string  `json:"conflicts,omitempty"`
	Head      string    `json:"head,omitempty"`
	StateAt   time.Time `json:"state_at,omitzero"`
}

type activityFile struct {
	Devices map[string]*DeviceActivity `json:"devices"`
}

// ReadActivity loads the activity recorded in dataDir. It is safe to call
// while the server runs.
func ReadActivity(dataDir string) (map[string]*DeviceActivity, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, ActivityFile))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]*DeviceActivity{}, nil
	}
	if err != nil {
		return nil, err
	}
	var af activityFile
	if err := json.Unmarshal(b, &af); err != nil {
		return nil, err
	}
	if af.Devices == nil {
		af.Devices = map[string]*DeviceActivity{}
	}
	return af.Devices, nil
}

// activity tracks device activity in memory and writes it to disk
// periodically, so busy servers don't rewrite the file on every request.
type activity struct {
	path  string
	log   *slog.Logger
	every time.Duration

	mu      sync.Mutex
	devices map[string]*DeviceActivity
	dirty   bool

	stop chan struct{}
	done chan struct{}
}

func newActivity(dataDir string, log *slog.Logger, every time.Duration) *activity {
	a := &activity{
		path:  filepath.Join(dataDir, ActivityFile),
		log:   log,
		every: every,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	devices, err := ReadActivity(dataDir)
	if err != nil {
		log.Warn("ignoring unreadable activity file", "err", err)
		devices = map[string]*DeviceActivity{}
	}
	a.devices = devices
	go a.loop()
	return a
}

func (a *activity) loop() {
	defer close(a.done)
	t := time.NewTicker(a.every)
	defer t.Stop()
	for {
		select {
		case <-a.stop:
			a.flush()
			return
		case <-t.C:
			a.flush()
		}
	}
}

func (a *activity) close() {
	select {
	case <-a.stop:
	default:
		close(a.stop)
	}
	<-a.done
}

func (a *activity) flush() {
	a.mu.Lock()
	if !a.dirty {
		a.mu.Unlock()
		return
	}
	b, err := json.MarshalIndent(activityFile{Devices: a.devices}, "", "  ")
	a.dirty = false
	a.mu.Unlock()
	if err != nil {
		a.log.Error("encoding activity", "err", err)
		return
	}
	if err := writeFileAtomic(a.path, append(b, '\n'), 0o600); err != nil {
		a.log.Error("writing activity", "err", err)
		a.mu.Lock()
		a.dirty = true
		a.mu.Unlock()
	}
}

func (a *activity) device(name string) *DeviceActivity {
	d := a.devices[name]
	if d == nil {
		d = &DeviceActivity{}
		a.devices[name] = d
	}
	return d
}

func (d *DeviceActivity) repo(name string) *RepoActivity {
	if d.Repos == nil {
		d.Repos = map[string]*RepoActivity{}
	}
	r := d.Repos[name]
	if r == nil {
		r = &RepoActivity{}
		d.Repos[name] = r
	}
	return r
}

func (a *activity) seen(device, ip, version string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := a.device(device)
	d.LastSeen, d.LastIP = time.Now().UTC(), ip
	if version != "" {
		d.ClientVersion = version
	}
	a.dirty = true
}

func (a *activity) fetched(device, repo string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.device(device).repo(repo).LastFetch = time.Now().UTC()
	a.dirty = true
}

func (a *activity) pushed(device, repo, head string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.device(device).repo(repo)
	r.LastPush, r.PushHead = time.Now().UTC(), head
	a.dirty = true
}

func (a *activity) reported(device, repo string, st RepoActivity) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.device(device).repo(repo)
	r.State, r.Message, r.Head, r.StateAt = st.State, st.Message, st.Head, time.Now().UTC()
	r.Conflicts = st.Conflicts
	a.dirty = true
}

// snapshot returns a deep copy of the current activity.
func (a *activity) snapshot() map[string]*DeviceActivity {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]*DeviceActivity, len(a.devices))
	for name, d := range a.devices {
		cp := *d
		cp.Repos = make(map[string]*RepoActivity, len(d.Repos))
		for rn, r := range d.Repos {
			rc := *r
			rc.Conflicts = append([]string(nil), r.Conflicts...)
			cp.Repos[rn] = &rc
		}
		out[name] = &cp
	}
	return out
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
