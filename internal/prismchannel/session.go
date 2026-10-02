package prismchannel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The operator-controlled material provider owns browser session refresh and
// fresh one-use verification tickets. The gateway never mints those tokens.
// POST protocol is in RUNBOOK.md; no credentials are sent to the provider.
type materialResponse struct {
	Headers        map[string]string `json:"headers"`
	Metadata       map[string]any    `json:"metadata"`
	ConversationID string            `json:"conversation_id"`
	ExpiresAt      int64             `json:"expires_at"`
}

func (c *Client) material(ctx context.Context, operation string, cr Credential, s *slot, path string) (materialResponse, error) {
	var out materialResponse
	if c.cfg.MaterialURL == "" {
		return out, nil
	}
	body := map[string]any{"operation": operation, "account_id": cr.ID, "path": path}
	if s != nil {
		hash := sha256.Sum256([]byte(s.owner))
		body["owner_hash"] = fmt.Sprintf("%x", hash[:])
		body["slot_id"] = s.id
	}
	b, _ := json.Marshal(body)
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.MaterialURL, bytes.NewReader(b))
	if e != nil {
		return out, &Error{502, "material_request", ""}
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.MaterialBearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.MaterialBearer)
	}
	resp, e := c.http.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		return out, &Error{502, "material_transport", ""}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return out, &Error{503, "material_unavailable", ""}
	}
	b, e = io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if e != nil || len(b) > 1<<20 || json.Unmarshal(b, &out) != nil {
		return out, &Error{502, "material_json", ""}
	}
	return out, nil
}
func (c *Client) materialHeaders(ctx context.Context, cr Credential, path string) (http.Header, error) {
	if c.cfg.MaterialURL == "" {
		return nil, nil
	}
	s, _ := ctx.Value(slotContextKey{}).(*slot)
	mat, e := c.material(ctx, "headers", cr, s, path)
	if e != nil {
		return nil, e
	}
	h := make(http.Header)
	for name, value := range mat.Headers {
		name = http.CanonicalHeaderKey(name)
		if strings.ContainsAny(value, "\r\n\x00") {
			return nil, &Error{502, "material_header_value", ""}
		}
		switch name {
		case "Cookie", "User-Agent", "Openai-Sentinel-Token", "Accept-Language":
			h.Set(name, value)
		default:
			return nil, &Error{502, "material_header_name", ""}
		}
	}
	if strings.HasPrefix(path, "/api/") && h.Get("openai-sentinel-token") == "" {
		return nil, &Error{503, "material_verification_missing", ""}
	}
	if (cr.SessionToken == "" || cr.AccessToken == "") && h.Get("Cookie") == "" {
		return nil, &Error{503, "material_cookie_missing", ""}
	}
	return h, nil
}
func (c *Client) prepareMaterial(ctx context.Context, s *slot) error {
	mat, e := c.material(ctx, "prepare", s.credential, s, "")
	if e != nil {
		return e
	}
	project, _ := mat.Metadata["projectId"].(string)
	sandboxURL, _ := mat.Metadata["sandbox_url"].(string)
	token, _ := mat.Metadata["sandbox_token"].(string)
	userID, _ := mat.Metadata["userId"].(string)
	if project == "" || strings.ContainsAny(project, "/\\?#") || sandboxURL == "" || token == "" || mat.ConversationID == "" || userID == "" {
		return &Error{502, "material_context_incomplete", ""}
	}
	if s.credential.UserID != "" && s.credential.UserID != userID {
		return &Error{502, "material_identity_mismatch", ""}
	}
	// Captured context is an identity-bound unit. Reject mismatched fields; do
	// not repair it by overlaying locally generated identity or sandbox values.
	if raw, ok := mat.Metadata["codex_listen_snapshot"]; ok {
		b, _ := json.Marshal(raw)
		var encoded string
		if json.Unmarshal(b, &encoded) == nil {
			b = []byte(encoded)
		}
		var snapshot map[string]any
		if json.Unmarshal(b, &snapshot) != nil || snapshot == nil {
			return &Error{502, "material_snapshot_invalid", ""}
		}
		for name, expected := range map[string]string{"user_id": userID, "project_id": project, "conversation_id": mat.ConversationID, "sandbox_token": token} {
			if v, exists := snapshot[name]; exists && v != expected {
				return &Error{502, "material_identity_mismatch", ""}
			}
		}
		if v, exists := snapshot["sandbox_url"]; exists && strings.TrimRight(fmt.Sprint(v), "/") != strings.TrimRight(sandboxURL, "/") {
			return &Error{502, "material_identity_mismatch", ""}
		}
	}
	sb := sandbox{URL: sandboxURL, Token: token}
	if _, e = c.sandboxPath(sb, "token"); e != nil {
		return e
	}
	expires := time.Unix(mat.ExpiresAt, 0)
	if !expires.After(time.Now().Add(5 * time.Second)) {
		return &Error{503, "material_expired", ""}
	}
	s.project = project
	s.sandbox = sb
	s.metadata = mat.Metadata
	s.conversationID = mat.ConversationID
	s.expires = minTime(expires.Add(-5*time.Second), time.Now().Add(c.cfg.SandboxTTL))
	s.warm = true
	s.generation = uuid.NewString()
	if !s.unmetered {
		c.metrics.WarmSlots.Add(1)
	}
	return nil
}

func snapshotSession(snapshot map[string]any) string {
	session, _ := snapshot["codex_session_id"].(string)
	return session
}

// Prefer the upstream snapshot verbatim. Opaque poll state is kept separately.
// Older upstreams expose the continuity fields only inside turn_state.
func responseSnapshot(e envelope, previous map[string]any) map[string]any {
	candidates := []json.RawMessage{e.Snapshot}
	if e.Response != nil {
		candidates = append(candidates, e.Response.Payload.Snapshot)
	}
	for _, raw := range candidates {
		var snapshot map[string]any
		var encoded string
		if json.Unmarshal(raw, &encoded) == nil {
			raw = []byte(encoded)
		}
		if json.Unmarshal(raw, &snapshot) == nil && snapshotSession(snapshot) != "" {
			return snapshot
		}
	}
	var state map[string]any
	if json.Unmarshal(e.TurnState, &state) == nil && snapshotSession(state) != "" {
		snapshot := make(map[string]any)
		for k, v := range previous {
			snapshot[k] = v
		}
		for _, k := range []string{"user_id", "project_id", "conversation_id", "sandbox_url", "sandbox_token", "workspace_session_id", "codex_session_id", "last_turn_id", "endpoint_identity", "last_exec_at", "transcript_cursor", "created_at", "updated_at", "last_saved_at"} {
			if v, ok := state[k]; ok {
				snapshot[k] = v
			}
		}
		return snapshot
	}
	return previous
}
