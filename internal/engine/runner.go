package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"icloud-alias-manager/internal/apple"
	"icloud-alias-manager/internal/model"
	"icloud-alias-manager/internal/store"
)

type Runner struct {
	store      *store.Store
	authClient *apple.AuthClient
	hmeClient  *apple.HmeClient
	mu         sync.Mutex
	cancelFunc context.CancelFunc

	// SSE subscriber channels
	subMu sync.RWMutex
	subs  map[chan model.LogEvent]struct{}
}

func NewRunner(s *store.Store, auth *apple.AuthClient) *Runner {
	return &Runner{
		store:      s,
		authClient: auth,
		hmeClient:  apple.NewHmeClient(),
		subs:       make(map[chan model.LogEvent]struct{}),
	}
}

// Subscribe returns a channel that receives live LogEvents.
func (r *Runner) Subscribe() chan model.LogEvent {
	r.subMu.Lock()
	defer r.subMu.Unlock()
	ch := make(chan model.LogEvent, 100)
	r.subs[ch] = struct{}{}
	return ch
}

// Unsubscribe removes an SSE subscriber channel.
func (r *Runner) Unsubscribe(ch chan model.LogEvent) {
	r.subMu.Lock()
	defer r.subMu.Unlock()
	delete(r.subs, ch)
	close(ch)
}

func (r *Runner) broadcast(evt *model.LogEvent) {
	r.subMu.RLock()
	defer r.subMu.RUnlock()
	for ch := range r.subs {
		select {
		case ch <- *evt:
		default:
		}
	}
}

func (r *Runner) log(level, account, msg, alias string) {
	evt := r.store.AddLog(level, account, msg, alias)
	r.broadcast(evt)
}

// Start launches the batch alias creation queue with the provided config.
func (r *Runner) Start(cfg model.TaskConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	st := r.store.GetTaskState()
	if st.Running {
		return fmt.Errorf("批量任务已在运行中")
	}

	r.store.SetTaskConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	r.cancelFunc = cancel

	now := time.Now()
	r.store.UpdateTaskState(func(s *model.TaskState) {
		s.Running = true
		s.TargetTotalLimit = cfg.TargetTotalLimit
		s.TotalCreated = 0
		s.TotalFailed = 0
		s.StartedAt = &now
		s.StoppedAt = nil
		s.CurrentStatus = "正在运行"
	})

	r.log("info", "System", fmt.Sprintf("批量创建任务启动：目标上限 %d 个，单账号上限 %d 个，生成间隔 %d 秒",
		cfg.TargetTotalLimit, cfg.PerAccountLimit, cfg.IntervalSeconds), "")

	go r.runLoop(ctx, cfg)
	return nil
}

// Stop pauses or cancels the active batch creation task.
func (r *Runner) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cancelFunc != nil {
		r.cancelFunc()
		r.cancelFunc = nil
	}

	now := time.Now()
	r.store.UpdateTaskState(func(s *model.TaskState) {
		s.Running = false
		s.StoppedAt = &now
		s.CurrentStatus = "已停止"
	})

	r.log("warning", "System", "批量创建任务已被用户手动停止", "")
}

func (r *Runner) runLoop(ctx context.Context, cfg model.TaskConfig) {
	defer func() {
		r.mu.Lock()
		now := time.Now()
		r.store.UpdateTaskState(func(s *model.TaskState) {
			s.Running = false
			s.StoppedAt = &now
		})
		r.mu.Unlock()
	}()

	interval := time.Duration(cfg.IntervalSeconds) * time.Second
	if interval < 1*time.Second {
		interval = 2 * time.Second
	}

	cooldownDur := time.Duration(cfg.CooldownMinutes) * time.Minute
	if cooldownDur < 1*time.Minute {
		cooldownDur = 15 * time.Minute
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Check global target limit
		taskState := r.store.GetTaskState()
		if cfg.TargetTotalLimit > 0 && taskState.TotalCreated >= cfg.TargetTotalLimit {
			r.store.UpdateTaskState(func(s *model.TaskState) {
				s.CurrentStatus = fmt.Sprintf("已达成目标上限 (%d/%d)", s.TotalCreated, cfg.TargetTotalLimit)
			})
			r.log("success", "System", fmt.Sprintf("🎉 批量任务顺利完成！已达到设定的总目标别名上限 %d 个", cfg.TargetTotalLimit), "")
			return
		}

		// Pick next eligible account
		accounts := r.store.ListAccounts()
		eligibleAccount := r.pickEligibleAccount(accounts, cfg)
		if eligibleAccount == nil {
			// Check if all accounts are cooling down or reached limit
			hasCooling := false
			minCoolRemaining := 60 * time.Minute
			for _, acc := range accounts {
				if acc.Status == model.StatusCooldown && acc.CooldownUntil != nil {
					rem := time.Until(*acc.CooldownUntil)
					if rem > 0 {
						hasCooling = true
						if rem < minCoolRemaining {
							minCoolRemaining = rem
						}
					}
				}
			}

			if hasCooling {
				waitSec := int(minCoolRemaining.Seconds())
				if waitSec > 60 {
					waitSec = 60
				}
				r.store.UpdateTaskState(func(s *model.TaskState) {
					s.CurrentStatus = fmt.Sprintf("所有可用账号均在冷却中，等待 %d 秒后重试...", waitSec)
				})
				r.log("info", "System", fmt.Sprintf("当前可用账号均触碰 Apple 频控冷却中，休眠 %d 秒后继续检测...", waitSec), "")
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Duration(waitSec) * time.Second):
					continue
				}
			}

			// No eligible accounts and none cooling down -> All reached limit or expired
			r.store.UpdateTaskState(func(s *model.TaskState) {
				s.CurrentStatus = "无可用账号（所有账号均已达上限或凭据失效）"
			})
			r.log("warning", "System", "任务结束：所有账号均已达到设定上限或登录态不可用", "")
			return
		}

		// We have an eligible account
		acc := eligibleAccount
		r.store.UpdateTaskState(func(s *model.TaskState) {
			s.CurrentAccount = acc.Email
			s.CurrentStatus = fmt.Sprintf("正在为账号 %s 处理别名...", acc.Email)
		})
		r.store.UpdateAccountStatus(acc.ID, model.StatusRunning, "")

		// Check if account needs auto-login via CodeURL
		if acc.Status == model.StatusWaiting2FA || (acc.Cookie == "" && acc.CodeURL != "") {
			r.log("info", acc.Email, fmt.Sprintf("账号附带取码接口 (%s)，正在自动发起登录并获取 2FA 验证码...", acc.CodeURL), "")
			r.store.UpdateTaskState(func(s *model.TaskState) {
				s.CurrentStatus = fmt.Sprintf("正在自动获取 %s 的 2FA 验证码并登录...", acc.Email)
			})
			if err := r.authClient.AutoLoginWithCodeURL(ctx, acc); err != nil {
				r.store.UpdateAccountStatus(acc.ID, model.StatusError, err.Error())
				r.log("error", acc.Email, fmt.Sprintf("❌ 自动取码登录失败: %v", err), "")
				continue
			}
			r.store.AddOrUpdateAccount(acc)
			r.log("success", acc.Email, "✅ 自动取码成功并完成双重认证，登录态已就绪！", "")
		}

		r.log("info", acc.Email, fmt.Sprintf("正在发起候选别名生成 (当前账号已建 %d 个)...", acc.SessionCreated), "")

		// 1. Generate candidate
		candidateEmail, err := r.hmeClient.GenerateCandidate(ctx, acc)
		if err != nil {
			r.handleError(acc, err, cooldownDur)
			continue
		}

		r.log("info", acc.Email, fmt.Sprintf("获取到候选别名: %s，正在提交保留 (Reserve)...", candidateEmail), candidateEmail)

		// 2. Reserve
		prefix := cfg.LabelPrefix
		if prefix == "" {
			prefix = "alias"
		}
		label := fmt.Sprintf("%s_%d", prefix, acc.SessionCreated+1)
		note := cfg.Note
		if note == "" {
			note = "iCloud Alias Manager"
		}

		alias, err := r.hmeClient.ReserveEmail(ctx, acc, candidateEmail, label, note)
		if err != nil {
			r.handleError(acc, err, cooldownDur)
			continue
		}

		// Success!
		r.store.AddAlias(alias)
		r.store.UpdateAccountStatus(acc.ID, model.StatusIdle, "")

		totalCreated := 0
		r.store.UpdateTaskState(func(s *model.TaskState) {
			s.TotalCreated++
			totalCreated = s.TotalCreated
		})

		r.log("success", acc.Email, fmt.Sprintf("✅ 成功创建别名: %s (总进度: %d/%d)", alias.Email, totalCreated, cfg.TargetTotalLimit), alias.Email)

		// Check if this account has now reached its limit
		accountLimit := acc.MaxLimit
		if accountLimit <= 0 && cfg.PerAccountLimit > 0 {
			accountLimit = cfg.PerAccountLimit
		}
		if accountLimit > 0 && acc.SessionCreated >= accountLimit {
			r.store.UpdateAccountStatus(acc.ID, model.StatusLimitReached, fmt.Sprintf("已达单账号别名上限 %d", accountLimit))
			r.log("info", acc.Email, fmt.Sprintf("账号 %s 已达单账号上限 %d 个，将切换下一个账号", acc.Email, accountLimit), "")
		}

		// Interval sleep
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (r *Runner) pickEligibleAccount(accounts []*model.Account, cfg model.TaskConfig) *model.Account {
	now := time.Now()

	// If user specified AccountIDs, filter by them
	allowedMap := make(map[string]bool)
	if len(cfg.AccountIDs) > 0 {
		for _, id := range cfg.AccountIDs {
			allowedMap[id] = true
		}
	}

	for _, acc := range accounts {
		if len(allowedMap) > 0 && !allowedMap[acc.ID] {
			continue
		}

		// If account was cooling down and cooldown expired, restore to Idle
		if acc.Status == model.StatusCooldown && acc.CooldownUntil != nil && now.After(*acc.CooldownUntil) {
			r.store.UpdateAccountStatus(acc.ID, model.StatusIdle, "")
			acc.Status = model.StatusIdle
		}

		// Skip accounts that are not Idle or ready for auto 2FA
		isAuto2FAReady := acc.Status == model.StatusWaiting2FA && acc.CodeURL != ""
		if acc.Status != model.StatusIdle && acc.Status != model.StatusRunning && !isAuto2FAReady {
			continue
		}

		// Check per-account limit
		accountLimit := acc.MaxLimit
		if accountLimit <= 0 && cfg.PerAccountLimit > 0 {
			accountLimit = cfg.PerAccountLimit
		}

		if accountLimit > 0 && acc.SessionCreated >= accountLimit {
			r.store.UpdateAccountStatus(acc.ID, model.StatusLimitReached, fmt.Sprintf("已达单账号上限 %d", accountLimit))
			continue
		}

		// Found eligible account
		return acc
	}

	return nil
}

func (r *Runner) handleError(acc *model.Account, err error, cooldownDur time.Duration) {
	r.store.UpdateTaskState(func(s *model.TaskState) {
		s.TotalFailed++
	})

	if err == apple.ErrRateLimited || strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "-41015") {
		coolUntil := time.Now().Add(cooldownDur)
		reason := fmt.Sprintf("Apple 频率限制，冷却至 %s", coolUntil.Format("15:04:05"))
		r.store.SetAccountCooldown(acc.ID, coolUntil, reason)
		r.log("warning", acc.Email, fmt.Sprintf("⚠️ 触碰 Apple 频控 (429/-41015)，该账号进入冷却至 %s，自动轮换下一个账号", coolUntil.Format("15:04:05")), "")
		return
	}

	if err == apple.ErrSessionExpired || strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "403") {
		r.store.UpdateAccountStatus(acc.ID, model.StatusExpired, "登录态已失效")
		r.log("error", acc.Email, fmt.Sprintf("❌ 账号登录态失效: %v，请重新导入该账号", err), "")
		return
	}

	// General error
	r.store.UpdateAccountStatus(acc.ID, model.StatusError, err.Error())
	r.log("error", acc.Email, fmt.Sprintf("❌ 创建失败: %v", err), "")
}
