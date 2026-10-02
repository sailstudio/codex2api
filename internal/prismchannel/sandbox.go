package prismchannel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type sandbox struct {
	URL       string `json:"url"`
	Token     string `json:"token"`
	SessionID string `json:"sandbox_session_id"`
}

func (c *Client) invalidate(s *slot) {
	if s.shared != nil {
		s.shared.mu.Lock()
		if s.shared.validGeneration == s.generation {
			s.shared.validGeneration = ""
		}
		s.shared.mu.Unlock()
	}
	if s.warm {
		s.warm = false
		if !s.unmetered {
			c.metrics.WarmSlots.Add(-1)
		}
	}
	s.expires = time.Time{}
}
func (c *Client) sandboxPath(sb sandbox, sub string) (string, error) {
	u, e := url.Parse(sb.URL)
	base, _ := url.Parse(c.cfg.BaseURL)
	if e != nil || u.Host != "" && (u.Host != base.Host || u.Scheme != base.Scheme) || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, "/") {
		return "", &Error{502, "sandbox_origin", ""}
	}
	return strings.TrimRight(u.Path, "/") + "/" + sub, nil
}
func (c *Client) prepare(ctx context.Context, s *slot, owner string) error {
	if s.shared == nil {
		return c.prepareExclusive(ctx, s, owner)
	}
	shared := s.shared
	select {
	case shared.gate <- struct{}{}:
		defer func() { <-shared.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if owner == "" {
		owner = shared.base.owner
	}
	shared.mu.Lock()
	valid := shared.validGeneration == shared.base.generation
	shared.mu.Unlock()
	if !valid {
		c.invalidate(shared.base)
	}
	if s.owner == owner && s.project != "" && s.project != shared.base.project && s.warm && time.Now().Before(s.expires) {
		return nil
	}
	previousGeneration := shared.base.generation
	if err := c.prepareExclusive(ctx, shared.base, owner); err != nil {
		return err
	}
	base := shared.base
	shared.mu.Lock()
	if base.generation != previousGeneration {
		shared.validGeneration = base.generation
	}
	valid = shared.validGeneration == base.generation
	shared.mu.Unlock()
	if !valid {
		return &Error{400, "previous_response_context_expired", ""}
	}
	// Owner changes allocate a new project/context; sibling turns retain their
	// old context until completion. Continuations validate the original project.
	if s.project != base.project {
		s.conversationID = ""
	}
	s.owner, s.project, s.sandbox, s.metadata, s.expires = base.owner, base.project, base.sandbox, base.metadata, base.expires
	s.generation = base.generation
	if !s.warm {
		s.warm = true
		c.metrics.WarmSlots.Add(1)
	}
	return nil
}

func (c *Client) prepareExclusive(ctx context.Context, s *slot, owner string) (err error) {
	if s.owner != "" && s.owner != owner {
		c.invalidate(s)
		s.project = ""
		s.sandbox = sandbox{}
		s.metadata = nil
		s.conversationID = ""
	}
	if owner != "" {
		s.owner = owner
	}
	if s.warm && time.Now().Before(s.expires) {
		return nil
	}
	c.invalidate(s)
	c.metrics.Warmups.Add(1)
	defer func() {
		if err != nil {
			c.metrics.WarmupErrors.Add(1)
		}
	}()
	if c.cfg.MaterialURL != "" && !c.cfg.MaterialHeadersOnly {
		return c.prepareMaterial(ctx, s)
	}
	if s.project == "" {
		id := uuid.NewString()
		var raw json.RawMessage
		if err = c.json(ctx, s, http.MethodPost, "/api/projects", map[string]any{"project_uuid": id, "title": "codex2api Prism channel"}, &raw, ""); err != nil {
			return err
		}
		// Successful creations may return a UUID string, a project object, or no ID.
		var returned string
		var obj struct {
			UUID    string `json:"uuid"`
			ID      string `json:"id"`
			Project struct {
				UUID string `json:"uuid"`
				ID   string `json:"id"`
			} `json:"project"`
		}
		if json.Unmarshal(raw, &returned) == nil && returned != "" {
			id = returned
		} else if json.Unmarshal(raw, &obj) == nil {
			for _, v := range []string{obj.UUID, obj.ID, obj.Project.UUID, obj.Project.ID} {
				if v != "" {
					id = v
					break
				}
			}
		}
		if strings.ContainsAny(id, "/\\?#") {
			return &Error{502, "project_id", ""}
		}
		s.project = id
	}
	if err = c.json(ctx, s, http.MethodPost, "/api/backend/1/new", map[string]any{}, &s.sandbox, ""); err != nil {
		return err
	}
	if s.sandbox.Token == "" || s.sandbox.URL == "" {
		return &Error{502, "sandbox_credentials", ""}
	}
	var rt struct {
		AccessToken string `json:"access_token"`
		Base        string `json:"resources_base_url"`
		Expires     int64  `json:"expires_at"`
		MaxAge      int64  `json:"max_age_seconds"`
	}
	var sandboxSessionID any
	if s.sandbox.SessionID != "" {
		sandboxSessionID = s.sandbox.SessionID
	}
	if err = c.json(ctx, s, http.MethodPost, "/api/projects/"+url.PathEscape(s.project)+"/sandbox/resources-token", map[string]any{"sandbox_session_id": sandboxSessionID, "sandbox_token": s.sandbox.Token}, &rt, ""); err != nil {
		return err
	}
	if rt.AccessToken == "" {
		return &Error{502, "resource_token", ""}
	}
	if rt.Base == "" {
		rt.Base = strings.TrimRight(c.cfg.BaseURL, "/") + "/s/sandbox-resources"
	}
	path, err := c.sandboxPath(s.sandbox, "resources-token")
	if err != nil {
		return err
	}
	if err = c.json(ctx, s, http.MethodPost, path, map[string]any{"token": rt.AccessToken, "resourceBaseUrl": rt.Base, "projectId": s.project}, nil, s.sandbox.Token); err != nil {
		return err
	}
	var y json.RawMessage
	if err = c.json(ctx, s, http.MethodPost, "/api/y", map[string]any{"docId": s.project, "requestContext": map[string]any{"source": "initial-bootstrap", "requestSeriesId": "prism-" + uuid.NewString(), "maxAttempts": 3}}, &y, ""); err != nil {
		return err
	}
	var ys struct {
		Token string `json:"token"`
		URL   string `json:"url"`
	}
	if json.Unmarshal(y, &ys) != nil || ys.Token == "" || ys.URL == "" {
		return &Error{502, "ysweet_token", ""}
	}
	path, err = c.sandboxPath(s.sandbox, "token")
	if err != nil {
		return err
	}
	if err = c.json(ctx, s, http.MethodPost, path, y, nil, s.sandbox.Token); err != nil {
		return err
	}
	path, err = c.sandboxPath(s.sandbox, "wait-for-sync?wait_ms=10000")
	if err != nil {
		return err
	}
	for {
		var st struct {
			Status       string   `json:"status"`
			Capabilities []string `json:"readinessCapabilities"`
			Tokens       struct {
				Y        bool `json:"hasCurrentYSweetToken"`
				Provider bool `json:"hasSyncedYSweetProvider"`
			} `json:"tokens"`
		}
		err = c.json(ctx, s, http.MethodGet, path, nil, &st, s.sandbox.Token)
		var ae *Error
		if errors.As(err, &ae) && (ae.Status == 404 || ae.Status == 501) {
			break
		}
		if err != nil {
			return err
		}
		if st.Status == "failed" {
			return &Error{502, "sandbox_sync_failed", ""}
		}
		needProvider := false
		for _, v := range st.Capabilities {
			if v == "current_y_sweet_provider" {
				needProvider = true
			}
		}
		if st.Status == "synced" && st.Tokens.Y && (!needProvider || st.Tokens.Provider) {
			break
		}
		if err = sleep(ctx, c.cfg.PollMax); err != nil {
			return err
		}
	}
	s.expires = time.Now().Add(c.cfg.SandboxTTL)
	if rt.Expires > 0 {
		s.expires = minTime(s.expires, time.Unix(rt.Expires, 0).Add(-30*time.Second))
	}
	if rt.MaxAge > 0 {
		s.expires = minTime(s.expires, time.Now().Add(time.Duration(rt.MaxAge)*time.Second-30*time.Second))
	}
	if !time.Now().Before(s.expires) {
		return &Error{502, "resource_token_expired", ""}
	}
	s.warm = true
	s.generation = uuid.NewString()
	if !s.unmetered {
		c.metrics.WarmSlots.Add(1)
	}
	return nil
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Prewarm reserves only idle slots and releases each as soon as it is ready.
// Two workers bound startup load on a single-account upstream.
func (c *Client) prewarm() {
	var idle []*slot
	for i := 0; i < min(c.cfg.PrewarmSlots, cap(c.slots)); i++ {
		select {
		case s := <-c.slots:
			idle = append(idle, s)
		default:
		}
	}
	jobs := make(chan *slot, len(idle))
	for _, s := range idle {
		jobs <- s
	}
	close(jobs)
	var wg sync.WaitGroup
	for i := 0; i < min(2, len(idle)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range jobs {
				ctx, cancel := context.WithTimeout(c.ctx, min(time.Minute, c.cfg.RequestTimeout))
				if s.account.unavailableUntil.Load() <= time.Now().UnixNano() {
					_ = c.prepare(ctx, s, "")
				}
				cancel()
				c.slots <- s
			}
		}()
	}
	wg.Wait()
}

// Keepalive refreshes idle slots; it never steals busy slots.
func (c *Client) keepWarm() {
	defer c.wg.Done()
	c.prewarm()
	t := time.NewTicker(c.cfg.KeepWarm)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			seen := make(map[*slot]bool)
			for i := 0; i < cap(c.slots); i++ {
				select {
				case s := <-c.slots:
					if seen[s] {
						c.slots <- s
						break
					}
					seen[s] = true
					if (s.project != "" || c.cfg.PrewarmSlots > 0) && s.account.unavailableUntil.Load() <= time.Now().UnixNano() {
						ctx, cancel := context.WithTimeout(c.ctx, min(30*time.Second, c.cfg.RequestTimeout))
						_ = c.prepare(ctx, s, s.owner)
						// The sync probe touches a valid idle container without reallocation.
						if s.warm {
							if path, e := c.sandboxPath(s.sandbox, "wait-for-sync?wait_ms=1000"); e == nil {
								if c.json(ctx, s, http.MethodGet, path, nil, nil, s.sandbox.Token) != nil {
									c.invalidate(s)
								}
							}
						}
						cancel()
					}
					c.slots <- s
				default:
				}
			}
		}
	}
}
