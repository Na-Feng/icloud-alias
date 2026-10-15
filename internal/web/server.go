package web

import (
	"embed"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"icloud-alias-manager/internal/apple"
	"icloud-alias-manager/internal/engine"
	"icloud-alias-manager/internal/model"
	"icloud-alias-manager/internal/store"
)

//go:embed static/*
var staticFS embed.FS

type Server struct {
	store      *store.Store
	runner     *engine.Runner
	authClient *apple.AuthClient
	hmeClient  *apple.HmeClient
	mux        *http.ServeMux
}

func NewServer(s *store.Store, r *engine.Runner, auth *apple.AuthClient) *Server {
	srv := &Server{
		store:      s,
		runner:     r,
		authClient: auth,
		hmeClient:  apple.NewHmeClient(),
		mux:        http.NewServeMux(),
	}
	srv.registerRoutes()
	return srv
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Enable CORS for development
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) registerRoutes() {
	// APIs
	s.mux.HandleFunc("/api/stats", s.handleStats)
	s.mux.HandleFunc("/api/accounts", s.handleAccounts)
	s.mux.HandleFunc("/api/accounts/import", s.handleImport)
	s.mux.HandleFunc("/api/accounts/login", s.handleLogin)
	s.mux.HandleFunc("/api/accounts/auto-login", s.handleAutoLogin)
	s.mux.HandleFunc("/api/accounts/2fa", s.handleSubmit2FA)
	s.mux.HandleFunc("/api/accounts/update-limit", s.handleUpdateLimit)
	s.mux.HandleFunc("/api/accounts/delete", s.handleDeleteAccount)
	s.mux.HandleFunc("/api/accounts/test", s.handleTestAccount)
	s.mux.HandleFunc("/api/aliases", s.handleAliases)
	s.mux.HandleFunc("/api/aliases/export", s.handleExportAliases)
	s.mux.HandleFunc("/api/task/start", s.handleTaskStart)
	s.mux.HandleFunc("/api/task/stop", s.handleTaskStop)
	s.mux.HandleFunc("/api/task/state", s.handleTaskState)
	s.mux.HandleFunc("/api/events", s.handleSSE)
	s.mux.HandleFunc("/api/logs", s.handleLogs)
	s.mux.HandleFunc("/api/logs/clear", s.handleClearLogs)

	// Static UI
	sub, err := fs.Sub(staticFS, "static")
	if err == nil {
		s.mux.Handle("/", http.FileServer(http.FS(sub)))
	}
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats := s.store.GetStats()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "stats": stats})
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	accounts := s.store.ListAccounts()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "accounts": accounts})
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "读取导入内容失败"})
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	// Try parsing json body, else use raw body as content
	if err := json.Unmarshal(body, &req); err != nil || req.Content == "" {
		req.Content = string(body)
	}

	res := apple.SmartBatchImport(req.Content)
	for _, acc := range res.Accounts {
		s.store.AddOrUpdateAccount(acc)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":         true,
		"total_processed": res.TotalProcessed,
		"success_count":   res.SuccessCount,
		"failure_count":   res.FailureCount,
		"errors":          res.Errors,
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "无效的请求参数"})
		return
	}

	pendingID, needs2FA, msg, err := s.authClient.StartLogin(r.Context(), req.Email, req.Password)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"pending_id": pendingID,
		"needs_2fa":  needs2FA,
		"message":    msg,
	})
}

func (s *Server) handleAutoLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "无效的请求参数"})
		return
	}

	acc, ok := s.store.GetAccount(req.ID)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "账号不存在"})
		return
	}

	if acc.CodeURL == "" {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "该账号未配置取码接口 (code_url)"})
		return
	}

	s.store.UpdateAccountStatus(acc.ID, model.StatusRunning, "正在自动获取验证码并登录...")
	if err := s.authClient.AutoLoginWithCodeURL(r.Context(), acc); err != nil {
		s.store.UpdateAccountStatus(acc.ID, model.StatusError, err.Error())
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
		return
	}

	s.store.AddOrUpdateAccount(acc)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "自动获取验证码并完成 2FA 认证成功！已生成可用登录态",
	})
}

func (s *Server) handleSubmit2FA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		PendingID string `json:"pending_id"`
		Code      string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "无效的请求参数"})
		return
	}

	acc, err := s.authClient.Submit2FA(r.Context(), req.PendingID, req.Code)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
		return
	}

	s.store.AddOrUpdateAccount(acc)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"account": acc,
		"message": "2FA 验证通过，已成功保存账号登录态！",
	})
}

func (s *Server) handleUpdateLimit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID       string `json:"id"`
		MaxLimit int    `json:"max_limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "无效的请求参数"})
		return
	}

	if err := s.store.UpdateAccountLimit(req.ID, req.MaxLimit); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "无效参数"})
		return
	}

	ok := s.store.DeleteAccount(req.ID)
	writeJSON(w, http.StatusOK, map[string]any{"success": ok})
}

func (s *Server) handleTestAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "无效参数"})
		return
	}

	acc, ok := s.store.GetAccount(req.ID)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "账号不存在"})
		return
	}

	count, err := s.hmeClient.CheckSession(r.Context(), acc)
	if err != nil {
		s.store.UpdateAccountStatus(acc.ID, model.StatusExpired, err.Error())
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
		return
	}

	acc.CurrentCount = count
	s.store.UpdateAccountStatus(acc.ID, model.StatusIdle, "")
	writeJSON(w, http.StatusOK, map[string]any{
		"success":       true,
		"current_count": count,
		"message":       fmt.Sprintf("登录态有效！检测到当前已有 %d 个别名", count),
	})
}

func (s *Server) handleAliases(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account_id")
	search := r.URL.Query().Get("search")
	aliases := s.store.ListAliases(accountID, search)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"total":   len(aliases),
		"aliases": aliases,
	})
}

func (s *Server) handleExportAliases(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account_id")
	format := strings.ToLower(r.URL.Query().Get("format"))
	aliases := s.store.ListAliases(accountID, "")

	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="icloud_aliases.csv"`)
		// Add UTF-8 BOM for Excel
		w.Write([]byte{0xEF, 0xBB, 0xBF})
		writer := csv.NewWriter(w)
		writer.Write([]string{"别名邮箱", "所属Apple账号", "标签", "备注", "转发目标", "创建时间"})
		for _, a := range aliases {
			writer.Write([]string{
				a.Email,
				a.AccountEmail,
				a.Label,
				a.Note,
				a.ForwardToEmail,
				a.CreatedAt.Format("2006-01-02 15:04:05"),
			})
		}
		writer.Flush()
		return
	}

	// Plain text format
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="icloud_aliases.txt"`)
	for _, a := range aliases {
		w.Write([]byte(a.Email + "\n"))
	}
}

func (s *Server) handleTaskStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var cfg model.TaskConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "无效配置"})
		return
	}

	if err := s.runner.Start(cfg); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "批量创建任务已启动"})
}

func (s *Server) handleTaskStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.runner.Stop()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "任务已停止"})
}

func (s *Server) handleTaskState(w http.ResponseWriter, r *http.Request) {
	st := s.store.GetTaskState()
	cfg := s.store.GetTaskConfig()
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"state":   st,
		"config":  cfg,
	})
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	logChan := s.runner.Subscribe()
	defer s.runner.Unsubscribe(logChan)

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Initial message
	fmt.Fprintf(w, "event: connected\ndata: {}\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case evt, ok := <-logChan:
			if !ok {
				return
			}
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)

			// Also push stats
			statsData, _ := json.Marshal(s.store.GetStats())
			fmt.Fprintf(w, "event: stats\ndata: %s\n\n", statsData)
			flusher.Flush()
		case <-ticker.C:
			// Heartbeat & stats sync
			statsData, _ := json.Marshal(s.store.GetStats())
			fmt.Fprintf(w, "event: stats\ndata: %s\n\n", statsData)
			flusher.Flush()
		}
	}
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if val, err := strconv.Atoi(l); err == nil && val > 0 {
			limit = val
		}
	}
	logs := s.store.GetRecentLogs(limit)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "logs": logs})
}

func (s *Server) handleClearLogs(w http.ResponseWriter, r *http.Request) {
	s.store.ClearLogs()
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func writeJSON(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}
