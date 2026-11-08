package relaycheckclient

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	dataFileName  = "data.enc"
	keyFileName   = "key.sealed"
	lockFileName  = "data.lock"
	maxHistory    = 2000
	staleLockAge  = 60 * time.Second
	lockWait      = 15 * time.Second
	sealedKeySize = 32
	// maxModelsPerTarget bounds what one click can spend.
	maxModelsPerTarget = 20
)

// Settings are the user's. Every default below can be changed on the page.
type Settings struct {
	ScheduleEnabled    bool              `json:"schedule_enabled"`
	IntervalHours      int               `json:"interval_hours"`
	MonthlyBudgetUSD   float64           `json:"monthly_budget_usd"`
	OfficialDailyLimit int               `json:"official_daily_limit"`
	AssumedCostUSD     float64           `json:"assumed_cost_usd"`
	Selected           []string          `json:"selected"`
	Models             map[string]string `json:"models"`
	// ModelSets are the models ticked per channel; empty means the one in use.
	ModelSets map[string][]string `json:"model_sets"`
}

// DefaultSettings are the first-run values.
func DefaultSettings() Settings {
	return Settings{IntervalHours: 6, MonthlyBudgetUSD: 5, OfficialDailyLimit: 4, AssumedCostUSD: 0.1, Models: map[string]string{}}
}

func (s *Settings) normalize() {
	if s.IntervalHours < 1 {
		s.IntervalHours = 1
	}
	if s.IntervalHours > 168 {
		s.IntervalHours = 168
	}
	if s.MonthlyBudgetUSD < 0 {
		s.MonthlyBudgetUSD = 0
	}
	if s.OfficialDailyLimit < 0 {
		s.OfficialDailyLimit = 0
	}
	if s.AssumedCostUSD <= 0 {
		s.AssumedCostUSD = DefaultSettings().AssumedCostUSD
	}
	if s.Models == nil {
		s.Models = map[string]string{}
	}
	if s.ModelSets == nil {
		s.ModelSets = map[string][]string{}
	}
	for id, models := range s.ModelSets {
		if len(models) > maxModelsPerTarget {
			s.ModelSets[id] = models[:maxModelsPerTarget]
		}
	}
}

// Data is everything this tool keeps. No API key is ever part of it.
type Data struct {
	Settings Settings `json:"settings"`
	History  []Result `json:"history"`
}

// Store keeps Data encrypted under a key the operating system protects. The
// scheduled run and the page can be different processes, so every write holds
// the lock file.
type Store struct {
	dir  string
	seal keySealer
}

// keySealer protects the store key at rest. Level says how, for the page.
type keySealer interface {
	Seal(key []byte) ([]byte, error)
	Unseal(sealed []byte) ([]byte, error)
	Level() string
}

// OpenStore opens the store in dir, creating it when missing.
func OpenStore(dir string) (*Store, error) {
	return openStore(dir, newPlatformSealer(dir))
}

func openStore(dir string, seal keySealer) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir, seal: seal}, nil
}

// DefaultStoreDir is the per-user data directory.
func DefaultStoreDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "PinAICheck"), nil
}

// SealLevel says how the data is protected.
func (s *Store) SealLevel() string { return s.seal.Level() }

func (s *Store) key() ([]byte, error) {
	path := filepath.Join(s.dir, keyFileName)
	sealed, err := os.ReadFile(path)
	if err == nil {
		key, unsealErr := s.seal.Unseal(sealed)
		if unsealErr != nil {
			return nil, fmt.Errorf("解不开本机密钥：%w", unsealErr)
		}
		if len(key) != sealedKeySize {
			return nil, errors.New("本机密钥长度不对")
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	key := make([]byte, sealedKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	sealed, err = s.seal.Seal(key)
	if err != nil {
		return nil, fmt.Errorf("保护本机密钥失败：%w", err)
	}
	if err := writeFileAtomic(path, sealed); err != nil {
		return nil, err
	}
	return key, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Load reads the data. A file this user's key cannot open is moved aside, not
// deleted: the tool starts fresh and the old file stays for recovery.
func (s *Store) Load() (Data, error) {
	data := Data{Settings: DefaultSettings()}
	raw, err := os.ReadFile(filepath.Join(s.dir, dataFileName))
	if os.IsNotExist(err) {
		return data, nil
	}
	if err != nil {
		return data, err
	}
	plain, err := s.decrypt(raw)
	if err == nil {
		err = json.Unmarshal(plain, &data)
	}
	if err != nil {
		aside := filepath.Join(s.dir, fmt.Sprintf("data.unreadable-%d.enc", time.Now().Unix()))
		_ = os.Rename(filepath.Join(s.dir, dataFileName), aside)
		return Data{Settings: DefaultSettings()}, nil
	}
	data.Settings.normalize()
	return data, nil
}

func (s *Store) decrypt(raw []byte) ([]byte, error) {
	key, err := s.key()
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, errors.New("数据文件太短")
	}
	return gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], []byte(dataFileName))
}

func (s *Store) save(data Data) error {
	if len(data.History) > maxHistory {
		data.History = data.History[len(data.History)-maxHistory:]
	}
	plain, err := json.Marshal(data)
	if err != nil {
		return err
	}
	key, err := s.key()
	if err != nil {
		return err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, dataFileName), gcm.Seal(nonce, nonce, plain, []byte(dataFileName)))
}

// Update is one locked read-change-write.
func (s *Store) Update(change func(*Data) error) (Data, error) {
	release, err := s.lock()
	if err != nil {
		return Data{}, err
	}
	defer release()
	data, err := s.Load()
	if err != nil {
		return data, err
	}
	if err := change(&data); err != nil {
		return data, err
	}
	data.Settings.normalize()
	return data, s.save(data)
}

func (s *Store) lock() (func(), error) {
	path := filepath.Join(s.dir, lockFileName)
	deadline := time.Now().Add(lockWait)
	for {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = file.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		// A crashed process must not lock the tool out for good.
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > staleLockAge {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, errors.New("本机数据被另一个进程占用")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func writeFileAtomic(path string, data []byte) error {
	dir, base := filepath.Split(path)
	tmp, err := os.CreateTemp(dir, strings.TrimPrefix(base, ".")+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
