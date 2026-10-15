package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"icloud-alias-manager/internal/model"
)

type Store struct {
	mu         sync.RWMutex
	filePath   string
	accounts   map[string]*model.Account
	aliases    []*model.Alias
	taskConfig model.TaskConfig
	taskState  model.TaskState
	logs       []*model.LogEvent
	logSeq     int64
}

type storeData struct {
	Accounts   []*model.Account  `json:"accounts"`
	Aliases    []*model.Alias    `json:"aliases"`
	TaskConfig model.TaskConfig  `json:"task_config"`
	Logs       []*model.LogEvent `json:"logs"`
}

func NewStore(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}

	filePath := filepath.Join(dataDir, "store.json")
	s := &Store{
		filePath: filePath,
		accounts: make(map[string]*model.Account),
		aliases:  make([]*model.Alias, 0),
		logs:     make([]*model.LogEvent, 0),
		taskConfig: model.TaskConfig{
			TargetTotalLimit: 50,
			PerAccountLimit:  5,
			IntervalSeconds:  3,
			CooldownMinutes:  15,
			LabelPrefix:      "alias",
			Note:             "Created by iCloud Alias Manager",
		},
		taskState: model.TaskState{
			Running:       false,
			CurrentStatus: "空闲",
		},
	}

	// Load existing file if it exists
	if _, err := os.Stat(filePath); err == nil {
		if err := s.load(); err != nil {
			fmt.Printf("加载现有数据警告: %v\n", err)
		}
	}

	return s, nil
}

func (s *Store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.filePath)
	if err != nil {
		return err
	}

	var d storeData
	if err := json.Unmarshal(data, &d); err != nil {
		return err
	}

	for _, acc := range d.Accounts {
		s.accounts[acc.ID] = acc
	}
	s.aliases = d.Aliases
	if d.TaskConfig.TargetTotalLimit > 0 {
		s.taskConfig = d.TaskConfig
	}
	if len(d.Logs) > 0 {
		s.logs = d.Logs
		if len(s.logs) > 500 {
			s.logs = s.logs[len(s.logs)-500:]
		}
		for _, l := range s.logs {
			if l.ID > s.logSeq {
				s.logSeq = l.ID
			}
		}
	}

	return nil
}

func (s *Store) saveLocked() error {
	var accList []*model.Account
	for _, acc := range s.accounts {
		accList = append(accList, acc)
	}

	// Keep max 500 logs on disk
	logList := s.logs
	if len(logList) > 500 {
		logList = logList[len(logList)-500:]
	}

	d := storeData{
		Accounts:   accList,
		Aliases:    s.aliases,
		TaskConfig: s.taskConfig,
		Logs:       logList,
	}

	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}

	tmpFile := s.filePath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpFile, s.filePath)
}

// Save persists the current in-memory data to disk.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

// Accounts

func (s *Store) AddOrUpdateAccount(acc *model.Account) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if account email already exists; if so, update existing record's cookies/credentials
	var existing *model.Account
	for _, a := range s.accounts {
		if a.Email == acc.Email {
			existing = a
			break
		}
	}

	if existing != nil {
		if acc.Cookie != "" {
			existing.Cookie = acc.Cookie
			existing.AuthType = acc.AuthType
			existing.Status = model.StatusIdle
		}
		if acc.Password != "" {
			existing.Password = acc.Password
		}
		if acc.Host != "" {
			existing.Host = acc.Host
		}
		if acc.DSID != "" {
			existing.DSID = acc.DSID
		}
		if acc.Scnt != "" {
			existing.Scnt = acc.Scnt
		}
		if acc.SessionToken != "" {
			existing.SessionToken = acc.SessionToken
		}
		existing.UpdatedAt = time.Now()
		_ = s.saveLocked()
		return
	}

	s.accounts[acc.ID] = acc
	_ = s.saveLocked()
}

func (s *Store) GetAccount(id string) (*model.Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	acc, ok := s.accounts[id]
	return acc, ok
}

func (s *Store) ListAccounts() []*model.Account {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var list []*model.Account
	for _, acc := range s.accounts {
		list = append(list, acc)
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt.After(list[j].CreatedAt)
	})
	return list
}

func (s *Store) DeleteAccount(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.accounts[id]; ok {
		delete(s.accounts, id)
		_ = s.saveLocked()
		return true
	}
	return false
}

func (s *Store) UpdateAccountLimit(id string, maxLimit int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	acc, ok := s.accounts[id]
	if !ok {
		return fmt.Errorf("账号 %s 不存在", id)
	}

	acc.MaxLimit = maxLimit
	acc.UpdatedAt = time.Now()
	return s.saveLocked()
}

func (s *Store) UpdateAccountStatus(id string, status model.AccountStatus, lastError string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	acc, ok := s.accounts[id]
	if !ok {
		return
	}
	acc.Status = status
	acc.LastError = lastError
	acc.UpdatedAt = time.Now()
	_ = s.saveLocked()
}

func (s *Store) SetAccountCooldown(id string, cooldownUntil time.Time, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	acc, ok := s.accounts[id]
	if !ok {
		return
	}
	acc.Status = model.StatusCooldown
	acc.CooldownUntil = &cooldownUntil
	acc.LastError = reason
	acc.UpdatedAt = time.Now()
	_ = s.saveLocked()
}

// Aliases

func (s *Store) AddAlias(alias *model.Alias) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.aliases = append(s.aliases, alias)

	// Increment account count
	if acc, ok := s.accounts[alias.AccountID]; ok {
		acc.CurrentCount++
		acc.SessionCreated++
		acc.UpdatedAt = time.Now()
	}

	_ = s.saveLocked()
}

func (s *Store) ListAliases(accountID string, search string) []*model.Alias {
	s.mu.RLock()
	defer s.mu.RUnlock()

	search = strings.ToLower(strings.TrimSpace(search))
	var result []*model.Alias

	for _, a := range s.aliases {
		if accountID != "" && a.AccountID != accountID {
			continue
		}
		if search != "" {
			match := strings.Contains(strings.ToLower(a.Email), search) ||
				strings.Contains(strings.ToLower(a.AccountEmail), search) ||
				strings.Contains(strings.ToLower(a.Label), search) ||
				strings.Contains(strings.ToLower(a.Note), search)
			if !match {
				continue
			}
		}
		result = append(result, a)
	}

	// Sort descending by created time
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}

// Task config and state

func (s *Store) GetTaskConfig() model.TaskConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.taskConfig
}

func (s *Store) SetTaskConfig(cfg model.TaskConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.taskConfig = cfg
	_ = s.saveLocked()
}

func (s *Store) GetTaskState() model.TaskState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.taskState
}

func (s *Store) UpdateTaskState(fn func(st *model.TaskState)) model.TaskState {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.taskState)
	return s.taskState
}

// Logs

func (s *Store) AddLog(level, account, message, alias string) *model.LogEvent {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.logSeq++
	event := &model.LogEvent{
		ID:        s.logSeq,
		Timestamp: time.Now(),
		Level:     level,
		Account:   account,
		Message:   message,
		Alias:     alias,
	}

	s.logs = append(s.logs, event)
	if len(s.logs) > 500 {
		s.logs = s.logs[len(s.logs)-500:]
	}
	return event
}

func (s *Store) GetRecentLogs(limit int) []*model.LogEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > len(s.logs) {
		limit = len(s.logs)
	}

	out := make([]*model.LogEvent, limit)
	copy(out, s.logs[len(s.logs)-limit:])
	return out
}

func (s *Store) ClearLogs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs = make([]*model.LogEvent, 0)
}

// Stats

func (s *Store) GetStats() model.Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	totalAccounts := len(s.accounts)
	activeAccounts := 0
	for _, acc := range s.accounts {
		if acc.Status == model.StatusIdle || acc.Status == model.StatusRunning {
			activeAccounts++
		}
	}

	totalAliases := len(s.aliases)
	todayAliases := 0
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	for _, a := range s.aliases {
		if a.CreatedAt.After(todayStart) {
			todayAliases++
		}
	}

	progress := 0
	target := s.taskConfig.TargetTotalLimit
	if target > 0 {
		progress = int(float64(s.taskState.TotalCreated) / float64(target) * 100)
		if progress > 100 {
			progress = 100
		}
	}

	return model.Stats{
		TotalAccounts:  totalAccounts,
		ActiveAccounts: activeAccounts,
		TotalAliases:   totalAliases,
		TodayAliases:   todayAliases,
		TaskRunning:    s.taskState.Running,
		TaskProgress:   progress,
		TaskTarget:     target,
	}
}
