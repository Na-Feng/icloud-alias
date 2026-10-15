package model

import "time"

// AuthType represents how the account authenticates.
type AuthType string

const (
	AuthTypeCurlCookie   AuthType = "curl_cookie"   // Imported from cURL or Cookie
	AuthTypeAppleAccount AuthType = "apple_account" // Apple Account protocol session
	AuthTypePassword2FA  AuthType = "password_2fa"  // Username + Password awaiting 2FA
)

// AccountStatus represents the current state of an account.
type AccountStatus string

const (
	StatusIdle         AccountStatus = "idle"          // Available to create aliases
	StatusRunning      AccountStatus = "running"       // Currently creating aliases
	StatusCooldown     AccountStatus = "cooldown"      // Rate-limited by Apple, cooling down
	StatusLimitReached AccountStatus = "limit_reached" // Reached configured alias creation limit
	StatusWaiting2FA   AccountStatus = "waiting_2fa"   // Waiting for 2FA code verification
	StatusExpired      AccountStatus = "expired"       // Session expired/invalid
	StatusError        AccountStatus = "error"         // Permanent error
)

// Account stores an imported Apple/iCloud account or session.
type Account struct {
	ID        string        `json:"id"`
	Email     string        `json:"email"`
	Password  string        `json:"password,omitempty"`
	CodeURL   string        `json:"code_url,omitempty"`   // Verification code fetching API endpoint
	AuthType  AuthType      `json:"auth_type"`
	Status    AccountStatus `json:"status"`
	Remark    string        `json:"remark"`

	// iCloud Web session details
	Host                  string `json:"host,omitempty"`                  // e.g. p68-maildomainws.icloud.com
	DSID                  string `json:"dsid,omitempty"`                  // Directory Services ID
	ClientID              string `json:"client_id,omitempty"`
	ClientBuildNumber     string `json:"client_build_number,omitempty"`
	ClientMasteringNumber string `json:"client_mastering_number,omitempty"`
	Cookie                string `json:"cookie,omitempty"`
	Origin                string `json:"origin,omitempty"`
	Referer               string `json:"referer,omitempty"`
	UserAgent             string `json:"user_agent,omitempty"`

	// Apple Account modern interface details
	Scnt         string `json:"scnt,omitempty"`
	SessionToken string `json:"session_token,omitempty"`
	APIKey       string `json:"api_key,omitempty"`

	// Limits & counters
	MaxLimit      int `json:"max_limit"`      // Max aliases to allow on this account (0 = unlimited / up to task limit)
	CurrentCount  int `json:"current_count"`  // Total existing aliases detected on Apple
	SessionCreated int `json:"session_created"` // Created in this session/task

	// Cooldown
	CooldownUntil *time.Time `json:"cooldown_until,omitempty"`
	LastError     string     `json:"last_error,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Alias represents a created Hide My Email (HME) alias.
type Alias struct {
	ID             string    `json:"id"`
	AccountID      string    `json:"account_id"`
	AccountEmail   string    `json:"account_email"`
	Email          string    `json:"email"`
	AnonymousID    string    `json:"anonymous_id"`
	Label          string    `json:"label"`
	Note           string    `json:"note"`
	ForwardToEmail string    `json:"forward_to_email"`
	IsActive       bool      `json:"is_active"`
	CreatedAt      time.Time `json:"created_at"`
}

// TaskConfig holds settings for batch alias creation.
type TaskConfig struct {
	TargetTotalLimit int      `json:"target_total_limit"` // Overall limit for this run (e.g. 50 aliases total)
	PerAccountLimit  int      `json:"per_account_limit"`  // Max aliases per individual account (e.g. 5)
	IntervalSeconds  int      `json:"interval_seconds"`   // Delay between creations in seconds (e.g. 3)
	CooldownMinutes  int      `json:"cooldown_minutes"`   // Cooldown period if Apple returns 429 (e.g. 15)
	LabelPrefix      string   `json:"label_prefix"`       // Prefix for alias label
	Note             string   `json:"note"`               // Note for created alias
	AccountIDs       []string `json:"account_ids"`        // Target accounts (empty = all available)
}

// TaskState tracks the live progress of a batch creation run.
type TaskState struct {
	Running          bool      `json:"running"`
	TargetTotalLimit int       `json:"target_total_limit"`
	TotalCreated     int       `json:"total_created"`     // Total newly created in this task
	TotalFailed      int       `json:"total_failed"`
	CurrentAccount   string    `json:"current_account"`
	CurrentStatus    string    `json:"current_status"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	StoppedAt        *time.Time `json:"stopped_at,omitempty"`
}

// LogEvent represents a live execution log entry sent to the frontend.
type LogEvent struct {
	ID        int64     `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"` // "info", "success", "warning", "error"
	Account   string    `json:"account"`
	Message   string    `json:"message"`
	Alias     string    `json:"alias,omitempty"`
}

// Stats holds high-level dashboard metrics.
type Stats struct {
	TotalAccounts  int `json:"total_accounts"`
	ActiveAccounts int `json:"active_accounts"`
	TotalAliases   int `json:"total_aliases"`
	TodayAliases   int `json:"today_aliases"`
	TaskRunning    bool `json:"task_running"`
	TaskProgress   int  `json:"task_progress"`
	TaskTarget     int  `json:"task_target"`
}
