package apple

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"icloud-alias-manager/internal/model"
)

var (
	ErrRateLimited    = errors.New("触碰 Apple 创建频率限制 (Rate Limited)")
	ErrSessionExpired = errors.New("账号登录态已失效 (Session Expired)")
	ErrAddressLimit   = errors.New("已达到 Apple 账号当前地址上限")
)

type HmeClient struct {
	httpClient *http.Client
}

func NewHmeClient() *HmeClient {
	return &HmeClient{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// GenerateCandidate generates a candidate HME email address from Apple.
func (c *HmeClient) GenerateCandidate(ctx context.Context, account *model.Account) (string, error) {
	if account.AuthType == model.AuthTypeAppleAccount && account.Scnt != "" && account.APIKey != "" {
		return c.generateAppleAccount(ctx, account)
	}
	return c.generateICloudWeb(ctx, account)
}

// ReserveEmail binds and reserves the generated HME address.
func (c *HmeClient) ReserveEmail(ctx context.Context, account *model.Account, hme, label, note string) (*model.Alias, error) {
	if account.AuthType == model.AuthTypeAppleAccount && account.Scnt != "" && account.APIKey != "" {
		return c.reserveAppleAccount(ctx, account, hme, label, note)
	}
	return c.reserveICloudWeb(ctx, account, hme, label, note)
}

// CheckSession checks if the account session is still valid by listing HME mailboxes.
func (c *HmeClient) CheckSession(ctx context.Context, account *model.Account) (int, error) {
	if account.AuthType == model.AuthTypeAppleAccount && account.Scnt != "" && account.APIKey != "" {
		return c.checkAppleAccount(ctx, account)
	}
	return c.checkICloudWeb(ctx, account)
}

// --- Classic iCloud Web Implementation ---

func (c *HmeClient) generateICloudWeb(ctx context.Context, account *model.Account) (string, error) {
	endpoint, err := c.buildICloudURL(account, "/v1/hme/generate")
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	c.applyICloudHeaders(req, account)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("网络请求错误: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusTooManyRequests {
		return "", ErrRateLimited
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", ErrSessionExpired
	}

	var parsed struct {
		Success bool `json:"success"`
		Result  struct {
			Hme string `json:"hme"`
		} `json:"result"`
		Error struct {
			Code   any    `json:"code"`
			Reason string `json:"reason"`
		} `json:"error"`
	}

	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		return "", fmt.Errorf("解析 Apple 响应失败 (HTTP %d): %s", resp.StatusCode, string(bodyBytes))
	}

	if !parsed.Success {
		reason := parsed.Error.Reason
		codeStr := fmt.Sprintf("%v", parsed.Error.Code)
		if codeStr == "-41015" || strings.Contains(strings.ToLower(reason), "limit") {
			return "", ErrRateLimited
		}
		if codeStr == "-21101" || strings.Contains(strings.ToLower(reason), "unauthorized") {
			return "", ErrSessionExpired
		}
		return "", fmt.Errorf("Apple 拒绝生成: %s (code: %s)", reason, codeStr)
	}

	if parsed.Result.Hme == "" {
		return "", errors.New("Apple 返回的候选别名为空")
	}

	return parsed.Result.Hme, nil
}

func (c *HmeClient) reserveICloudWeb(ctx context.Context, account *model.Account, hme, label, note string) (*model.Alias, error) {
	endpoint, err := c.buildICloudURL(account, "/v1/hme/reserve")
	if err != nil {
		return nil, err
	}

	payload := map[string]string{
		"hme":   hme,
		"label": label,
		"note":  note,
	}
	reqData, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqData))
	if err != nil {
		return nil, err
	}
	c.applyICloudHeaders(req, account)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("保留别名网络错误: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, ErrRateLimited
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrSessionExpired
	}

	var parsed struct {
		Success bool `json:"success"`
		Result  struct {
			Hme struct {
				AnonymousID    string `json:"anonymousId"`
				Hme            string `json:"hme"`
				Label          string `json:"label"`
				Note           string `json:"note"`
				ForwardToEmail string `json:"forwardToEmail"`
				IsActive       bool   `json:"isActive"`
			} `json:"hme"`
		} `json:"result"`
		Error struct {
			Code   any    `json:"code"`
			Reason string `json:"reason"`
		} `json:"error"`
	}

	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		return nil, fmt.Errorf("解析保留响应失败 (HTTP %d): %s", resp.StatusCode, string(bodyBytes))
	}

	if !parsed.Success {
		reason := parsed.Error.Reason
		codeStr := fmt.Sprintf("%v", parsed.Error.Code)
		if codeStr == "-41015" || strings.Contains(strings.ToLower(reason), "limit") {
			return nil, ErrRateLimited
		}
		if codeStr == "-21101" || strings.Contains(strings.ToLower(reason), "unauthorized") {
			return nil, ErrSessionExpired
		}
		return nil, fmt.Errorf("保留别名失败: %s (code: %s)", reason, codeStr)
	}

	alias := &model.Alias{
		ID:             fmt.Sprintf("alias_%d", time.Now().UnixNano()),
		AccountID:      account.ID,
		AccountEmail:   account.Email,
		Email:          parsed.Result.Hme.Hme,
		AnonymousID:    parsed.Result.Hme.AnonymousID,
		Label:          parsed.Result.Hme.Label,
		Note:           parsed.Result.Hme.Note,
		ForwardToEmail: parsed.Result.Hme.ForwardToEmail,
		IsActive:       parsed.Result.Hme.IsActive,
		CreatedAt:      time.Now(),
	}

	return alias, nil
}

func (c *HmeClient) checkICloudWeb(ctx context.Context, account *model.Account) (int, error) {
	endpoint, err := c.buildICloudURL(account, "/v2/hme/list")
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	c.applyICloudHeaders(req, account)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("检测网络错误: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return 0, ErrSessionExpired
	}

	var parsed struct {
		Success bool `json:"success"`
		Result  struct {
			HmeEmails []struct {
				AnonymousID string `json:"anonymousId"`
				Hme         string `json:"hme"`
			} `json:"hmeEmails"`
		} `json:"result"`
		Error struct {
			Code   any    `json:"code"`
			Reason string `json:"reason"`
		} `json:"error"`
	}

	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		return 0, fmt.Errorf("解析列表响应失败: %w", err)
	}

	if !parsed.Success {
		return 0, fmt.Errorf("列表返回错误: %s", parsed.Error.Reason)
	}

	return len(parsed.Result.HmeEmails), nil
}

func (c *HmeClient) buildICloudURL(account *model.Account, path string) (string, error) {
	host := strings.TrimSpace(account.Host)
	if host == "" {
		host = "p68-maildomainws.icloud.com"
	}
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		host = "https://" + host
	}

	u, err := url.Parse(host)
	if err != nil {
		return "", err
	}
	u.Path = path

	q := u.Query()
	if account.ClientBuildNumber != "" {
		q.Set("clientBuildNumber", account.ClientBuildNumber)
	} else {
		q.Set("clientBuildNumber", "2425Project73")
	}
	if account.ClientID != "" {
		q.Set("clientId", account.ClientID)
	}
	if account.ClientMasteringNumber != "" {
		q.Set("clientMasteringNumber", account.ClientMasteringNumber)
	} else {
		q.Set("clientMasteringNumber", "2425B61")
	}
	if account.DSID != "" {
		q.Set("dsid", account.DSID)
	}

	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (c *HmeClient) applyICloudHeaders(req *http.Request, account *model.Account) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	if account.Origin != "" {
		req.Header.Set("Origin", account.Origin)
	} else {
		req.Header.Set("Origin", "https://www.icloud.com")
	}
	if account.Referer != "" {
		req.Header.Set("Referer", account.Referer)
	} else {
		req.Header.Set("Referer", req.Header.Get("Origin")+"/")
	}
	if account.UserAgent != "" {
		req.Header.Set("User-Agent", account.UserAgent)
	} else {
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36")
	}
	if account.Cookie != "" {
		req.Header.Set("Cookie", account.Cookie)
	}
}

// --- Modern Apple Account Implementation ---

func (c *HmeClient) generateAppleAccount(ctx context.Context, account *model.Account) (string, error) {
	apiURL := "https://account.apple.com/api/v1/hme/generate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	c.applyAppleAccountHeaders(req, account)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 429 {
		return "", ErrRateLimited
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 419 {
		return "", ErrSessionExpired
	}

	body, _ := io.ReadAll(resp.Body)
	var parsed struct {
		Hme string `json:"hme"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Hme != "" {
		return parsed.Hme, nil
	}
	return "", fmt.Errorf("Apple Account 生成别名异常: %s", string(body))
}

func (c *HmeClient) reserveAppleAccount(ctx context.Context, account *model.Account, hme, label, note string) (*model.Alias, error) {
	apiURL := "https://account.apple.com/api/v1/hme/reserve"
	payload := map[string]string{
		"hme":   hme,
		"label": label,
		"note":  note,
	}
	bodyData, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(bodyData))
	if err != nil {
		return nil, err
	}
	c.applyAppleAccountHeaders(req, account)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 429 {
		return nil, ErrRateLimited
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 419 {
		return nil, ErrSessionExpired
	}

	body, _ := io.ReadAll(resp.Body)
	var parsed struct {
		AnonymousID    string `json:"anonymousId"`
		Hme            string `json:"hme"`
		Label          string `json:"label"`
		Note           string `json:"note"`
		ForwardToEmail string `json:"forwardToEmail"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Hme != "" {
		return &model.Alias{
			ID:             fmt.Sprintf("alias_%d", time.Now().UnixNano()),
			AccountID:      account.ID,
			AccountEmail:   account.Email,
			Email:          parsed.Hme,
			AnonymousID:    parsed.AnonymousID,
			Label:          parsed.Label,
			Note:           parsed.Note,
			ForwardToEmail: parsed.ForwardToEmail,
			IsActive:       true,
			CreatedAt:      time.Now(),
		}, nil
	}

	return nil, fmt.Errorf("Apple Account 保留别名失败: %s", string(body))
}

func (c *HmeClient) checkAppleAccount(ctx context.Context, account *model.Account) (int, error) {
	apiURL := "https://account.apple.com/api/v1/hme/list"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return 0, err
	}
	c.applyAppleAccountHeaders(req, account)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 419 {
		return 0, ErrSessionExpired
	}

	body, _ := io.ReadAll(resp.Body)
	var parsed struct {
		ForwardingList []struct {
			HmeList []any `json:"hmeList"`
		} `json:"forwardingList"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		total := 0
		for _, f := range parsed.ForwardingList {
			total += len(f.HmeList)
		}
		return total, nil
	}
	return 0, nil
}

func (c *HmeClient) applyAppleAccountHeaders(req *http.Request, account *model.Account) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Apple-Request-Context", "ca")
	req.Header.Set("X-Apple-I-FD-Client-Info", `{"platform":"macOS"}`)
	if account.Scnt != "" {
		req.Header.Set("scnt", account.Scnt)
	}
	if account.APIKey != "" {
		req.Header.Set("X-Apple-Api-Key", account.APIKey)
	}
	if account.Cookie != "" {
		req.Header.Set("Cookie", account.Cookie)
	}
	req.Header.Set("Origin", "https://account.apple.com")
	req.Header.Set("Referer", "https://account.apple.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36")
}
