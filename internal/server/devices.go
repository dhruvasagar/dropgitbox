package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// DevicesFile is the device registry inside the data directory.
const DevicesFile = "devices.json"

// tokenPrefix makes dropgitbox tokens recognisable (e.g. to secret scanners).
const tokenPrefix = "dgb_"

var validDevice = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidDeviceName reports whether name is acceptable as a device name.
func ValidDeviceName(name string) bool { return validDevice.MatchString(name) }

// Device is a registered client device. Only a hash of its token is kept.
type Device struct {
	Name      string    `json:"name"`
	TokenHash string    `json:"token_sha256"`
	Created   time.Time `json:"created"`
	Rotated   time.Time `json:"rotated,omitzero"`
}

type deviceFile struct {
	Devices []Device `json:"devices"`
}

// ErrDeviceExists is returned when adding a device name that is taken.
var ErrDeviceExists = errors.New("device already exists")

// ErrNoSuchDevice is returned when revoking an unknown device.
var ErrNoSuchDevice = errors.New("no such device")

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewToken returns a fresh random token.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// ListDevices returns the devices registered in dataDir.
func ListDevices(dataDir string) ([]Device, error) {
	df, err := readDevices(filepath.Join(dataDir, DevicesFile))
	if err != nil {
		return nil, err
	}
	return df.Devices, nil
}

// AddDevice registers a device in dataDir and returns its new token. The
// running server picks the change up without a restart.
func AddDevice(dataDir, name string) (string, error) {
	if !ValidDeviceName(name) {
		return "", fmt.Errorf("invalid device name %q (letters, digits, '.', '_' and '-' only)", name)
	}
	path := filepath.Join(dataDir, DevicesFile)
	df, err := readDevices(path)
	if err != nil {
		return "", err
	}
	if slices.ContainsFunc(df.Devices, func(d Device) bool { return d.Name == name }) {
		return "", fmt.Errorf("%w: %s (revoke it first to issue a new token)", ErrDeviceExists, name)
	}
	token, err := NewToken()
	if err != nil {
		return "", err
	}
	df.Devices = append(df.Devices, Device{Name: name, TokenHash: hashToken(token), Created: time.Now().UTC().Truncate(time.Second)})
	if err := writeDevices(dataDir, path, df); err != nil {
		return "", err
	}
	return token, nil
}

// RotateDevice replaces a device's token: the old one stops working
// immediately and the new one is returned. Use it when a token may have
// leaked but the device itself is fine.
func RotateDevice(dataDir, name string) (string, error) {
	path := filepath.Join(dataDir, DevicesFile)
	df, err := readDevices(path)
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(df.Devices, func(d Device) bool { return d.Name == name })
	if i < 0 {
		return "", fmt.Errorf("%w: %s", ErrNoSuchDevice, name)
	}
	token, err := NewToken()
	if err != nil {
		return "", err
	}
	df.Devices[i].TokenHash = hashToken(token)
	df.Devices[i].Rotated = time.Now().UTC().Truncate(time.Second)
	if err := writeDevices(dataDir, path, df); err != nil {
		return "", err
	}
	return token, nil
}

// RevokeDevice removes a device; its token stops working immediately.
func RevokeDevice(dataDir, name string) error {
	path := filepath.Join(dataDir, DevicesFile)
	df, err := readDevices(path)
	if err != nil {
		return err
	}
	n := len(df.Devices)
	df.Devices = slices.DeleteFunc(df.Devices, func(d Device) bool { return d.Name == name })
	if len(df.Devices) == n {
		return fmt.Errorf("%w: %s", ErrNoSuchDevice, name)
	}
	return writeDevices(dataDir, path, df)
}

func readDevices(path string) (*deviceFile, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &deviceFile{}, nil
	}
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return nil, fmt.Errorf("%w (run as the server's user, or with sudo)", err)
		}
		return nil, err
	}
	var df deviceFile
	if err := json.Unmarshal(b, &df); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &df, nil
}

func writeDevices(dataDir, path string, df *deviceFile) error {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(df, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dataDir, ".devices.*")
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf("%w (run as the server's user, or with sudo)", err)
		}
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// When an admin runs this with sudo, hand the file to the user that
	// owns the data directory (the server's user) so it can read it.
	if err := chownLike(tmp.Name(), dataDir); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// registry is the server's view of devices.json, reloaded whenever the
// file changes on disk.
type registry struct {
	path string

	mu     sync.Mutex
	fi     os.FileInfo
	byHash map[string]string
}

func newRegistry(dataDir string) *registry {
	return &registry{path: filepath.Join(dataDir, DevicesFile)}
}

// lookup returns the device a token belongs to.
func (r *registry) lookup(token string) (string, bool, error) {
	if !strings.HasPrefix(token, tokenPrefix) {
		return "", false, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.refresh(); err != nil {
		return "", false, err
	}
	name, ok := r.byHash[hashToken(token)]
	return name, ok, nil
}

// count returns the number of registered devices.
func (r *registry) count() (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.refresh(); err != nil {
		return 0, err
	}
	return len(r.byHash), nil
}

func (r *registry) refresh() error {
	fi, err := os.Stat(r.path)
	if errors.Is(err, fs.ErrNotExist) {
		r.fi, r.byHash = nil, nil
		return nil
	}
	if err != nil {
		return err
	}
	if r.fi != nil && os.SameFile(r.fi, fi) && r.fi.ModTime().Equal(fi.ModTime()) && r.fi.Size() == fi.Size() {
		return nil
	}
	df, err := readDevices(r.path)
	if err != nil {
		return err
	}
	byHash := make(map[string]string, len(df.Devices))
	for _, d := range df.Devices {
		byHash[d.TokenHash] = d.Name
	}
	r.fi, r.byHash = fi, byHash
	return nil
}
