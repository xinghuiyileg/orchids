package clerk

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type ClientResponse struct {
	Response struct {
		ID                  string `json:"id"`
		LastActiveSessionID string `json:"last_active_session_id"`
		Sessions            []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			User   struct {
				ID             string `json:"id"`
				EmailAddresses []struct {
					EmailAddress string `json:"email_address"`
				} `json:"email_addresses"`
			} `json:"user"`
			LastActiveToken struct {
				JWT string `json:"jwt"`
			} `json:"last_active_token"`
		} `json:"sessions"`
	} `json:"response"`
}

type AccountInfo struct {
	SessionID    string
	ClientCookie string
	ClientUat    string
	ProjectID    string
	UserID       string
	Email        string
	JWT          string
}

type CheckResult struct {
	Valid   bool
	Banned  bool
	Message string
}

func FetchAccountInfo(clientCookie string) (*AccountInfo, error) {
	url := "https://clerk.orchids.app/v1/client?__clerk_api_version=2025-11-10&_clerk_js_version=5.117.0"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Orchids/0.0.57 Chrome/138.0.7204.251 Electron/37.10.3 Safari/537.36")
	req.Header.Set("Accept-Language", "zh-CN")
	req.AddCookie(&http.Cookie{Name: "__client", Value: clientCookie})

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch client info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(body))
	}

	var clientResp ClientResponse
	if err := json.NewDecoder(resp.Body).Decode(&clientResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if len(clientResp.Response.Sessions) == 0 {
		return nil, fmt.Errorf("no active sessions found")
	}

	session := clientResp.Response.Sessions[0]
	if len(session.User.EmailAddresses) == 0 {
		return nil, fmt.Errorf("no email address found")
	}

	return &AccountInfo{
		SessionID:    clientResp.Response.LastActiveSessionID,
		ClientCookie: clientCookie,
		ClientUat:    fmt.Sprintf("%d", time.Now().Unix()),
		ProjectID:    "280b7bae-cd29-41e4-a0a6-7f603c43b607",
		UserID:       session.User.ID,
		Email:        session.User.EmailAddresses[0].EmailAddress,
		JWT:          session.LastActiveToken.JWT,
	}, nil
}

func CheckAccountStatus(sessionID, clientCookie, clientUat string) CheckResult {
	tokenURL := fmt.Sprintf("https://clerk.orchids.app/v1/client/sessions/%s/tokens?__clerk_api_version=2025-11-10&_clerk_js_version=5.117.0", sessionID)

	req, err := http.NewRequest("POST", tokenURL, strings.NewReader("organization_id="))
	if err != nil {
		return CheckResult{Valid: false, Message: "request error: " + err.Error()}
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", fmt.Sprintf("__client=%s; __client_uat=%s", clientCookie, clientUat))

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return CheckResult{Valid: false, Message: "network error: " + err.Error()}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK {
		var tokenResp struct {
			JWT string `json:"jwt"`
		}
		if json.Unmarshal(body, &tokenResp) == nil && tokenResp.JWT != "" {
			banned := checkUpstreamBan(tokenResp.JWT)
			if banned {
				return CheckResult{Valid: true, Banned: true, Message: "account banned by upstream"}
			}
			return CheckResult{Valid: true, Banned: false, Message: "ok"}
		}
	}

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return CheckResult{Valid: false, Banned: true, Message: fmt.Sprintf("session invalid: %d", resp.StatusCode)}
	}

	return CheckResult{Valid: false, Message: fmt.Sprintf("status %d: %s", resp.StatusCode, string(body))}
}

func checkUpstreamBan(jwt string) bool {
	req, err := http.NewRequest("POST", "https://orchids-server.calmstone-6964e08a.westeurope.azurecontainerapps.io/agent/coding-agent", strings.NewReader(`{"prompt":"hi","chatHistory":[],"projectId":"test","currentPage":{},"agentMode":"claude-sonnet-4-5","mode":"agent","gitRepoUrl":"","email":"test@test.com","chatSessionId":1,"userId":"test","apiVersion":2}`))
	if err != nil {
		return false
	}

	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Orchids-Api-Version", "2")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return true
	}
	return false
}
