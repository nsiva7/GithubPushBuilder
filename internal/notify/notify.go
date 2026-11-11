// Package notify sends Slack messages and GitHub commit statuses.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

var client = &http.Client{Timeout: 15 * time.Second}

// Slack posts a message to an incoming webhook. color is a hex colour for the
// side bar of the message.
func Slack(ctx context.Context, webhookURL, text, color string) error {
	body, _ := json.Marshal(map[string]any{
		"attachments": []map[string]any{{
			"color":     color,
			"text":      text,
			"mrkdwn_in": []string{"text"},
			"fallback":  text,
		}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return do(req)
}

// CommitStatus sets a status on a commit. state is pending, success, failure or error.
func CommitStatus(ctx context.Context, apiBase, token, repo, sha, state, description, targetURL, statusContext string) error {
	if len(description) > 140 { // GitHub's limit
		description = description[:137] + "..."
	}
	payload := map[string]string{"state": state, "description": description, "context": statusContext}
	if targetURL != "" {
		payload["target_url"] = targetURL
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/repos/%s/statuses/%s", apiBase, repo, sha)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return do(req)
}

func do(req *http.Request) error {
	resp, err := client.Do(req)
	if err != nil {
		// The URL can carry a secret (Slack webhook), so don't echo the full error.
		return fmt.Errorf("request to %s failed", req.URL.Host)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("%s answered %s: %s", req.URL.Host, resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}
