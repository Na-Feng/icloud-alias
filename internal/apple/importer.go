package apple

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"icloud-alias-manager/internal/model"
)

var (
	curlURLRegex = regexp.MustCompile(`(?i)curl\s+(?:--location\s+)?['"](https?://[^'"]+)['"]`)
	curlURLBareRegex = regexp.MustCompile(`(?i)curl\s+(https?://\S+)`)
	curlHeaderRegex = regexp.MustCompile(`(?i)-H\s+['"]([^'"]+)['"]`)
	curlCookieFlagRegex = regexp.MustCompile(`(?i)(?:-b|--cookie)\s+['"]([^'"]+)['"]`)
	hostRegex = regexp.MustCompile(`(?i)[a-z0-9](?:[a-z0-9-]*[a-z0-9])?-maildomainws\.icloud\.com(?:\.cn)?`)
)

type ImportResult struct {
	TotalProcessed int              `json:"total_processed"`
	SuccessCount   int              `json:"success_count"`
	FailureCount   int              `json:"failure_count"`
	Accounts       []*model.Account `json:"accounts"`
	Errors         []string         `json:"errors"`
}

// ParseHmeCurl extracts host, query params, cookies, and headers from a cURL command.
func ParseHmeCurl(curlText string) (*model.Account, error) {
	// Find URL
	var rawURL string
	if m := curlURLRegex.FindStringSubmatch(curlText); len(m) > 1 {
		rawURL = m[1]
	} else if m := curlURLBareRegex.FindStringSubmatch(curlText); len(m) > 1 {
		rawURL = m[1]
	} else {
		return nil, errors.New("未能在 cURL 文本中找到有效 URL")
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("解析 cURL URL 失败: %w", err)
	}

	host := strings.ToLower(u.Host)
	if !strings.Contains(host, "maildomainws.icloud.com") && !strings.Contains(host, "account.apple.com") {
		return nil, errors.New("cURL URL 必须指向 iCloud maildomainws 或 Apple Account 接口")
	}

	query := u.Query()
	dsid := query.Get("dsid")
	clientID := query.Get("clientId")
	buildNumber := query.Get("clientBuildNumber")
	masteringNumber := query.Get("clientMasteringNumber")

	// Extract headers
	headers := make(map[string]string)
	headerMatches := curlHeaderRegex.FindAllStringSubmatch(curlText, -1)
	for _, m := range headerMatches {
		if len(m) > 1 {
			parts := strings.SplitN(m[1], ":", 2)
			if len(parts) == 2 {
				k := strings.TrimSpace(parts[0])
				v := strings.TrimSpace(parts[1])
				headers[strings.ToLower(k)] = v
			}
		}
	}

	// Extract cookies (from header or -b / --cookie flag)
	cookie := headers["cookie"]
	if cookie == "" {
		if cm := curlCookieFlagRegex.FindStringSubmatch(curlText); len(cm) > 1 {
			cookie = cm[1]
		}
	}

	if cookie == "" {
		return nil, errors.New("cURL 请求中未包含 Cookie")
	}

	origin := headers["origin"]
	if origin == "" {
		if strings.Contains(host, ".cn") {
			origin = "https://www.icloud.com.cn"
		} else {
			origin = "https://www.icloud.com"
		}
	}

	userAgent := headers["user-agent"]
	if userAgent == "" {
		userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"
	}

	referer := headers["referer"]
	if referer == "" {
		referer = origin + "/"
	}

	// Extract account email identifier if present in cookies or dsid
	email := extractEmailFromCookie(cookie)
	if email == "" {
		if dsid != "" {
			email = "icloud_user_" + dsid
		} else {
			email = fmt.Sprintf("session_%d", time.Now().UnixNano()%100000)
		}
	}

	account := &model.Account{
		ID:                    fmt.Sprintf("acc_%d", time.Now().UnixNano()),
		Email:                 email,
		AuthType:              model.AuthTypeCurlCookie,
		Status:                model.StatusIdle,
		Host:                  host,
		DSID:                  dsid,
		ClientID:              clientID,
		ClientBuildNumber:     buildNumber,
		ClientMasteringNumber: masteringNumber,
		Cookie:                cookie,
		Origin:                origin,
		Referer:               referer,
		UserAgent:             userAgent,
		MaxLimit:              0, // 0 = unlimited or obey task limit
		CreatedAt:             time.Now(),
		UpdatedAt:             time.Now(),
	}

	return account, nil
}

// ParseRawCookie parses raw cookie text and constructs an Account with default host.
func ParseRawCookie(rawCookie string, host string) (*model.Account, error) {
	cookie := strings.TrimSpace(rawCookie)
	if cookie == "" {
		return nil, errors.New("Cookie 不能为空")
	}

	if host == "" {
		host = "p68-maildomainws.icloud.com"
	}

	email := extractEmailFromCookie(cookie)
	if email == "" {
		email = fmt.Sprintf("cookie_user_%d", time.Now().UnixNano()%100000)
	}

	origin := "https://www.icloud.com"
	if strings.Contains(host, ".cn") {
		origin = "https://www.icloud.com.cn"
	}

	account := &model.Account{
		ID:                    fmt.Sprintf("acc_%d", time.Now().UnixNano()),
		Email:                 email,
		AuthType:              model.AuthTypeCurlCookie,
		Status:                model.StatusIdle,
		Host:                  host,
		ClientID:              fmt.Sprintf("d39ba991%x", time.Now().UnixNano()),
		ClientBuildNumber:     "2425Project73",
		ClientMasteringNumber: "2425B61",
		Cookie:                cookie,
		Origin:                origin,
		Referer:               origin + "/",
		UserAgent:             "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
		MaxLimit:              0,
		CreatedAt:             time.Now(),
		UpdatedAt:             time.Now(),
	}

	return account, nil
}

// ParseCredentials parses credentials such as "email----password" or "email----password----remark"
func ParseCredentials(line string) (*model.Account, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, errors.New("行内容为空")
	}

	var parts []string
	if strings.Contains(line, "----") {
		parts = strings.Split(line, "----")
	} else if strings.Contains(line, ",") {
		parts = strings.Split(line, ",")
	} else if strings.Contains(line, ":") {
		parts = strings.Split(line, ":")
	} else {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			parts = fields
		}
	}

	var email, password, codeURL, remark string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
			codeURL = p
		} else if strings.Contains(p, "@") && email == "" {
			email = p
		} else if email != "" && password == "" {
			password = p
		} else if remark == "" {
			remark = p
		}
	}

	// Fallback to position-based if not matched
	if email == "" && len(parts) >= 1 {
		email = strings.TrimSpace(parts[0])
	}
	if password == "" && len(parts) >= 2 {
		password = strings.TrimSpace(parts[1])
	}
	if codeURL == "" && len(parts) >= 3 && (strings.HasPrefix(parts[2], "http://") || strings.HasPrefix(parts[2], "https://")) {
		codeURL = strings.TrimSpace(parts[2])
	}

	if email == "" || password == "" {
		return nil, errors.New("格式错误：需包含邮箱和密码（如 email----password----code_url）")
	}

	if !strings.Contains(email, "@") {
		return nil, fmt.Errorf("无效的邮箱格式: %s", email)
	}

	account := &model.Account{
		ID:        fmt.Sprintf("acc_%d", time.Now().UnixNano()),
		Email:     email,
		Password:  password,
		CodeURL:   codeURL,
		AuthType:  model.AuthTypePassword2FA,
		Status:    model.StatusWaiting2FA,
		Remark:    remark,
		MaxLimit:  0,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	return account, nil
}

// SmartBatchImport parses any imported string content into accounts.
func SmartBatchImport(content string) *ImportResult {
	res := &ImportResult{
		Accounts: make([]*model.Account, 0),
		Errors:   make([]string, 0),
	}

	content = strings.TrimSpace(content)
	if content == "" {
		res.Errors = append(res.Errors, "导入内容为空")
		return res
	}

	// 1. Try JSON array of accounts
	if strings.HasPrefix(content, "[") && strings.HasSuffix(content, "]") {
		var list []map[string]any
		if err := json.Unmarshal([]byte(content), &list); err == nil {
			for idx, item := range list {
				res.TotalProcessed++
				email, _ := item["email"].(string)
				pwd, _ := item["password"].(string)
				cookie, _ := item["cookie"].(string)
				if email != "" && cookie != "" {
					acc, err := ParseRawCookie(cookie, "")
					if err == nil {
						acc.Email = email
						res.Accounts = append(res.Accounts, acc)
						res.SuccessCount++
						continue
					}
				}
				if email != "" && pwd != "" {
					acc, err := ParseCredentials(fmt.Sprintf("%s----%s", email, pwd))
					if err == nil {
						res.Accounts = append(res.Accounts, acc)
						res.SuccessCount++
						continue
					}
				}
				res.FailureCount++
				res.Errors = append(res.Errors, fmt.Sprintf("JSON 第 %d 项解析失败", idx+1))
			}
			return res
		}
	}

	// 2. Try splitting by cURL commands
	if strings.Contains(content, "curl ") {
		// Split by "curl "
		curlBlocks := splitCurlBlocks(content)
		if len(curlBlocks) > 0 {
			for idx, block := range curlBlocks {
				res.TotalProcessed++
				acc, err := ParseHmeCurl(block)
				if err != nil {
					res.FailureCount++
					res.Errors = append(res.Errors, fmt.Sprintf("cURL #%d 解析失败: %v", idx+1, err))
				} else {
					res.SuccessCount++
					res.Accounts = append(res.Accounts, acc)
				}
			}
			return res
		}
	}

	// 3. Line by line parsing
	lines := strings.Split(content, "\n")
	for idx, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}

		res.TotalProcessed++

		// Check if it's a cookie line
		if strings.Contains(line, "X-APPLE-WEBAUTH") || strings.Contains(line, "X-APPLE-DS-WEB") {
			acc, err := ParseRawCookie(line, "")
			if err != nil {
				res.FailureCount++
				res.Errors = append(res.Errors, fmt.Sprintf("第 %d 行 Cookie 解析失败: %v", idx+1, err))
			} else {
				res.SuccessCount++
				res.Accounts = append(res.Accounts, acc)
			}
			continue
		}

		// Otherwise parse as credentials (email----password)
		acc, err := ParseCredentials(line)
		if err != nil {
			res.FailureCount++
			res.Errors = append(res.Errors, fmt.Sprintf("第 %d 行解析失败: %v", idx+1, err))
		} else {
			res.SuccessCount++
			res.Accounts = append(res.Accounts, acc)
		}
	}

	return res
}

func extractEmailFromCookie(cookie string) string {
	// Look for user email inside cookie e.g. X-APPLE-WEBAUTH-USER="xxx@icloud.com" or dslang etc
	re := regexp.MustCompile(`(?i)(?:user|account|email)=["']?([a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,})["']?`)
	if m := re.FindStringSubmatch(cookie); len(m) > 1 {
		return m[1]
	}
	return ""
}

func splitCurlBlocks(content string) []string {
	var blocks []string
	lines := strings.Split(content, "\n")
	var current strings.Builder
	inCurl := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "curl ") {
			if inCurl && current.Len() > 0 {
				blocks = append(blocks, current.String())
				current.Reset()
			}
			inCurl = true
			current.WriteString(line + "\n")
		} else if inCurl {
			current.WriteString(line + "\n")
		}
	}
	if inCurl && current.Len() > 0 {
		blocks = append(blocks, current.String())
	}
	return blocks
}
