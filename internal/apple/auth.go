package apple

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"icloud-alias-manager/internal/model"
)

const (
	appleAuthUserAgent              = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.3.1 Safari/605.1.15"
	defaultAppleOAuthClientID       = "d39ba9916b7251055b22c7f910e2ea796ee65e98b2ddecea8f5dde8d9d1a815d"
	appleAccountManageOAuthClientID = "af1139274f266b22b68c2a3e7ad932cb3c0bbe854e13a79af78dcc73136882c3"
)

type AuthClient struct {
	httpClient *http.Client
	pendingMu  sync.Mutex
	pendings   map[string]*pendingAuthSession
}

func NewAuthClient() *AuthClient {
	return &AuthClient{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		pendings:   make(map[string]*pendingAuthSession),
	}
}

type pendingAuthSession struct {
	ID        string
	Email     string
	Password  string
	Host      string
	Session   *appleAuthSession
	CreatedAt time.Time
	ExpiresAt time.Time
}

type appleAuthEndpoints struct {
	Home  string
	Setup string
	Auth  string
	Host  string
}

type appleAuthSession struct {
	Endpoints       appleAuthEndpoints
	AppleID         string
	ClientID        string
	FrameID         string
	UserAgent       string
	SessionToken    string
	Scnt            string
	SessionID       string
	AccountCountry  string
	TrustToken      string
	TwoFactorMethod string
	Cookies         map[string]*http.Cookie
}

func (s *appleAuthSession) CookieHeader() string {
	var parts []string
	for _, c := range s.Cookies {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

func (s *appleAuthSession) ExtractCookies(resp *http.Response) {
	if s.Cookies == nil {
		s.Cookies = make(map[string]*http.Cookie)
	}
	for _, c := range resp.Cookies() {
		s.Cookies[c.Name] = c
	}
	if scnt := resp.Header.Get("scnt"); scnt != "" {
		s.Scnt = scnt
	}
	if sid := resp.Header.Get("X-Apple-ID-Session-Id"); sid != "" {
		s.SessionID = sid
	}
	if st := resp.Header.Get("X-Apple-Session-Token"); st != "" {
		s.SessionToken = st
	}
	if tt := resp.Header.Get("X-Apple-TwoSV-Trust-Token"); tt != "" {
		s.TrustToken = tt
	}
}

// StartLogin initiates SRP signin and determines if 2FA code is needed.
func (c *AuthClient) StartLogin(ctx context.Context, email, password string) (string, bool, string, error) {
	email = strings.TrimSpace(email)
	password = strings.TrimSpace(password)
	if email == "" || password == "" {
		return "", false, "", errors.New("邮箱和密码不能为空")
	}

	host := "www.icloud.com"
	endpoints := appleAuthEndpoints{
		Home:  "https://www.icloud.com",
		Setup: "https://setup.icloud.com/setup/ws/1",
		Auth:  "https://idmsa.apple.com/appleauth/auth",
		Host:  host,
	}

	frameID := fmt.Sprintf("%x-%x", time.Now().UnixNano(), randBytes(4))
	session := &appleAuthSession{
		Endpoints: endpoints,
		AppleID:   email,
		ClientID:  defaultAppleOAuthClientID,
		FrameID:   frameID,
		UserAgent: appleAuthUserAgent,
		Cookies:   make(map[string]*http.Cookie),
	}

	// 1. Authorize signin
	if err := c.authStart(ctx, session); err != nil {
		return "", false, "", fmt.Errorf("初始化 Apple 授权失败: %w", err)
	}

	// 2. Federate
	if err := c.authFederate(ctx, session); err != nil {
		return "", false, "", fmt.Errorf("Apple 联邦查询失败: %w", err)
	}

	// 3. SRP
	needs2FA, err := c.authSRP(ctx, session, password)
	if err != nil {
		return "", false, "", fmt.Errorf("Apple SRP 握手失败: %w", err)
	}

	pendingID := fmt.Sprintf("pend_%d", time.Now().UnixNano())
	now := time.Now()
	pending := &pendingAuthSession{
		ID:        pendingID,
		Email:     email,
		Password:  password,
		Host:      host,
		Session:   session,
		CreatedAt: now,
		ExpiresAt: now.Add(10 * time.Minute),
	}

	c.pendingMu.Lock()
	c.pendings[pendingID] = pending
	c.pendingMu.Unlock()

	if needs2FA {
		_ = c.requestTrustedDeviceCode(ctx, session)
		return pendingID, true, "已触发受信任设备验证码，请在 Apple 设备允许后输入 6 位验证码", nil
	}

	// Direct login succeeded
	return pendingID, false, "登录成功，无需 2FA", nil
}

// Submit2FA verifies the 6-digit code and creates the Account model.
func (c *AuthClient) Submit2FA(ctx context.Context, pendingID, code string) (*model.Account, error) {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return nil, errors.New("验证码必须为 6 位数字")
	}

	c.pendingMu.Lock()
	pending, ok := c.pendings[pendingID]
	c.pendingMu.Unlock()

	if !ok || time.Now().After(pending.ExpiresAt) {
		return nil, errors.New("2FA 会话已过期，请重新发起登录")
	}

	session := pending.Session

	// 1. Validate security code
	body := map[string]any{
		"securityCode": map[string]string{
			"code": code,
		},
	}
	headers := c.srpHeaders(session)
	headers["Accept"] = "application/json"

	_, _, err := c.do(ctx, session, http.MethodPost, session.Endpoints.Auth+"/verify/trusteddevice/securitycode", headers, body, nil)
	if err != nil {
		return nil, fmt.Errorf("验证码校验错误: %w", err)
	}

	// 2. Trust session
	_, _, _ = c.do(ctx, session, http.MethodGet, session.Endpoints.Auth+"/2sv/trust", c.authHeaders(session), nil, nil)

	// 3. Exchange for iCloud webservice accountLogin
	accountLoginURL := session.Endpoints.Setup + "/accountLogin"
	loginBody := map[string]any{
		"accountCountryCode": session.AccountCountry,
		"dsWebAuthToken":     session.SessionToken,
		"extended_login":     true,
		"trustToken":         session.TrustToken,
	}

	var accountInfo struct {
		DSInfo struct {
			DSID         string `json:"dsid"`
			AppleID      string `json:"appleId"`
			PrimaryEmail string `json:"primaryEmail"`
		} `json:"dsInfo"`
		Webservices map[string]struct {
			URL    string `json:"url"`
			Status string `json:"status"`
		} `json:"webservices"`
	}

	loginHeaders := c.authHeaders(session)
	_, _, err = c.do(ctx, session, http.MethodPost, accountLoginURL, loginHeaders, loginBody, &accountInfo)
	if err != nil {
		return nil, fmt.Errorf("换取 iCloud 会话失败: %w", err)
	}

	cookieStr := session.CookieHeader()
	dsid := accountInfo.DSInfo.DSID

	// Extract maildomain host from webservices if available
	host := "p68-maildomainws.icloud.com"
	if ws, exists := accountInfo.Webservices["maildomain"]; exists && ws.URL != "" {
		if u, err := url.Parse(ws.URL); err == nil && u.Host != "" {
			host = u.Host
		}
	}

	acc := &model.Account{
		ID:                    fmt.Sprintf("acc_%d", time.Now().UnixNano()),
		Email:                 pending.Email,
		AuthType:              model.AuthTypePassword2FA,
		Status:                model.StatusIdle,
		Host:                  host,
		DSID:                  dsid,
		ClientID:              session.ClientID,
		ClientBuildNumber:     "2425Project73",
		ClientMasteringNumber: "2425B61",
		Cookie:                cookieStr,
		Scnt:                  session.Scnt,
		SessionToken:          session.SessionToken,
		Origin:                session.Endpoints.Home,
		Referer:               session.Endpoints.Home + "/",
		UserAgent:             session.UserAgent,
		MaxLimit:              0,
		CreatedAt:             time.Now(),
		UpdatedAt:             time.Now(),
	}

	// Remove from pending
	c.pendingMu.Lock()
	delete(c.pendings, pendingID)
	c.pendingMu.Unlock()

	return acc, nil
}

func (c *AuthClient) authStart(ctx context.Context, session *appleAuthSession) error {
	frameTag := "auth-" + session.FrameID
	u, err := url.Parse(session.Endpoints.Auth + "/authorize/signin")
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("frame_id", frameTag)
	q.Set("skVersion", "7")
	q.Set("iframeId", frameTag)
	q.Set("client_id", session.ClientID)
	q.Set("redirect_uri", session.Endpoints.Home)
	q.Set("response_type", "code")
	q.Set("response_mode", "web_message")
	q.Set("state", frameTag)
	q.Set("language", "zh_CN")
	q.Set("authVersion", "latest")
	u.RawQuery = q.Encode()

	headers := map[string]string{"Accept": "*/*"}
	_, _, err = c.do(ctx, session, http.MethodGet, u.String(), headers, nil, nil)
	return err
}

func (c *AuthClient) authFederate(ctx context.Context, session *appleAuthSession) error {
	u := session.Endpoints.Auth + "/federate?isRememberMeEnabled=true"
	body := map[string]any{"accountName": session.AppleID, "rememberMe": true}
	_, _, err := c.do(ctx, session, http.MethodPost, u, c.srpHeaders(session), body, nil)
	return err
}

func (c *AuthClient) authSRP(ctx context.Context, session *appleAuthSession, password string) (bool, error) {
	srp, err := newSRPClient()
	if err != nil {
		return false, err
	}

	var initResp struct {
		Iteration int    `json:"iteration"`
		Salt      string `json:"salt"`
		Protocol  string `json:"protocol"`
		B         string `json:"b"`
		C         string `json:"c"`
	}

	initBody := map[string]any{
		"a":           base64.StdEncoding.EncodeToString(srp.ABytes()),
		"accountName": session.AppleID,
		"protocols":   []string{"s2k", "s2k_fo"},
	}

	if _, _, err := c.do(ctx, session, http.MethodPost, session.Endpoints.Auth+"/signin/init", c.srpHeaders(session), initBody, &initResp); err != nil {
		return false, err
	}

	serverB, err := base64.StdEncoding.DecodeString(initResp.B)
	if err != nil {
		return false, fmt.Errorf("解析 SRP B 失败: %w", err)
	}

	salt, err := base64.StdEncoding.DecodeString(initResp.Salt)
	if err != nil {
		return false, fmt.Errorf("解析 SRP Salt 失败: %w", err)
	}

	derived, err := deriveAppleSRPPassword(password, salt, initResp.Iteration, initResp.Protocol)
	if err != nil {
		return false, err
	}

	if err := srp.processChallenge([]byte(session.AppleID), derived, salt, serverB); err != nil {
		return false, err
	}

	completeBody := map[string]any{
		"accountName": session.AppleID,
		"m1":          base64.StdEncoding.EncodeToString(srp.M1),
		"m2":          base64.StdEncoding.EncodeToString(srp.M2),
		"c":           initResp.C,
		"rememberMe":  true,
		"trustTokens": []string{},
	}

	status, _, err := c.do(ctx, session, http.MethodPost, session.Endpoints.Auth+"/signin/complete?isRememberMeEnabled=true", c.srpHeaders(session), completeBody, nil)
	if status == http.StatusUnauthorized {
		return false, errors.New("Apple ID 或密码错误")
	}
	if status == http.StatusConflict { // HTTP 409 means 2FA challenge required!
		return true, nil
	}
	return false, err
}

func (c *AuthClient) requestTrustedDeviceCode(ctx context.Context, session *appleAuthSession) error {
	headers := c.srpHeaders(session)
	_, _, err := c.do(ctx, session, http.MethodPut, session.Endpoints.Auth+"/verify/trusteddevice/securitycode", headers, nil, nil)
	return err
}

func (c *AuthClient) do(ctx context.Context, session *appleAuthSession, method, rawURL string, headers map[string]string, body any, out any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return 0, nil, err
	}

	req.Header.Set("User-Agent", session.UserAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie := session.CookieHeader(); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	session.ExtractCookies(resp)
	data, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusConflict {
		return http.StatusConflict, data, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, data, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data))
	}

	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		_ = json.Unmarshal(data, out)
	}

	return resp.StatusCode, data, nil
}

func (c *AuthClient) srpHeaders(session *appleAuthSession) map[string]string {
	h := map[string]string{
		"Accept":               "application/json, text/javascript, */*; q=0.01",
		"Content-Type":         "application/json",
		"X-Apple-OAuth-Client": session.ClientID,
		"X-Requested-With":     "XMLHttpRequest",
	}
	if session.Scnt != "" {
		h["scnt"] = session.Scnt
	}
	if session.SessionID != "" {
		h["X-Apple-ID-Session-Id"] = session.SessionID
	}
	return h
}

func (c *AuthClient) authHeaders(session *appleAuthSession) map[string]string {
	h := map[string]string{
		"Accept":           "application/json, text/javascript, */*; q=0.01",
		"Content-Type":     "application/json",
		"Origin":           session.Endpoints.Home,
		"Referer":          session.Endpoints.Home + "/",
		"X-Requested-With": "XMLHttpRequest",
	}
	if session.Scnt != "" {
		h["scnt"] = session.Scnt
	}
	if session.SessionID != "" {
		h["X-Apple-ID-Session-Id"] = session.SessionID
	}
	return h
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

var sixDigitCodeRegex = regexp.MustCompile(`\b(\d{6})\b`)

// FetchCodeFromURL polls the given URL until a 6-digit verification code different from oldCode is returned or timeout.
func (c *AuthClient) FetchCodeFromURL(ctx context.Context, codeURL string, oldCode string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("等待验证码超时 (超过 %v)", timeout)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, codeURL, nil)
		if err == nil {
			req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
			resp, err := c.httpClient.Do(req)
			if err == nil {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				matches := sixDigitCodeRegex.FindStringSubmatch(string(body))
				if len(matches) > 1 {
					code := matches[1]
					if oldCode == "" || code != oldCode {
						return code, nil
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// PeekCodeFromURL inspects the current code on the endpoint.
func (c *AuthClient) PeekCodeFromURL(codeURL string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codeURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	m := sixDigitCodeRegex.FindStringSubmatch(string(body))
	if len(m) > 1 {
		return m[1]
	}
	return ""
}

// AutoLoginWithCodeURL automatically logs into Apple ID, polls codeURL for 2FA, and saves session.
func (c *AuthClient) AutoLoginWithCodeURL(ctx context.Context, acc *model.Account) error {
	if acc.CodeURL == "" {
		return errors.New("账号未配置验证码获取接口 (code_url)")
	}

	oldCode := c.PeekCodeFromURL(acc.CodeURL)

	pendingID, needs2FA, _, err := c.StartLogin(ctx, acc.Email, acc.Password)
	if err != nil {
		return fmt.Errorf("发起登录失败: %w", err)
	}

	if needs2FA {
		code, err := c.FetchCodeFromURL(ctx, acc.CodeURL, oldCode, 60*time.Second)
		if err != nil {
			return fmt.Errorf("自动获取验证码失败: %w", err)
		}

		loggedAcc, err := c.Submit2FA(ctx, pendingID, code)
		if err != nil {
			return fmt.Errorf("提交验证码失败: %w", err)
		}

		acc.Cookie = loggedAcc.Cookie
		acc.Host = loggedAcc.Host
		acc.DSID = loggedAcc.DSID
		acc.Scnt = loggedAcc.Scnt
		acc.SessionToken = loggedAcc.SessionToken
		acc.Origin = loggedAcc.Origin
		acc.Referer = loggedAcc.Referer
		acc.UserAgent = loggedAcc.UserAgent
		acc.Status = model.StatusIdle
		acc.LastError = ""
		acc.UpdatedAt = time.Now()
		return nil
	}

	acc.Status = model.StatusIdle
	acc.LastError = ""
	acc.UpdatedAt = time.Now()
	return nil
}
