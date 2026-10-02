package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/prismchannel"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

func (h *Handler) ClosePrism() {
	if h.prismClient != nil {
		h.prismClient.Close()
	}
}

// The standard auth middleware has already run. Branch before stock model
// validation. Prism credentials have no stock account/group/plan mapping, so
// account-scoped policies must fail closed rather than bypass stock scheduling.
func (h *Handler) handlePrism(c *gin.Context, body []byte, responses bool) bool {
	model := gjson.GetBytes(body, "model").String()
	force := h.cfg != nil && h.cfg.Prism.Enabled && h.cfg.Prism.Force
	if !strings.HasPrefix(model, "prism-") && !force {
		return false
	}
	if h.prismClient == nil {
		prismError(c, http.StatusServiceUnavailable, "prism_unconfigured")
		return true
	}
	if requestUpstreamChannel(c) != database.UpstreamChannelAuto {
		prismError(c, http.StatusForbidden, "api_key_channel_restricted")
		return true
	}
	if row := apiKeyRowFromContext(c); row != nil &&
		(len(row.AllowedGroupIDs) > 0 || len(row.Limits.PlanAllow) > 0 ||
			len(row.Limits.NoAffinityGroupIDs) > 0 || len(row.Limits.ScopeLimits) > 0) {
		prismError(c, http.StatusForbidden, "prism_account_policy_unsupported")
		return true
	}
	if len(body) > security.MaxRequestBodySize {
		prismError(c, 413, "request_too_large")
		return true
	}
	if security.ValidateModelName(model) != nil || strings.TrimPrefix(model, "prism-") == "" {
		prismError(c, 400, "invalid_model")
		return true
	}
	r, err := prismchannel.Decode(body, responses)
	if err != nil {
		prismError(c, 400, err.Error())
		return true
	}
	endpoint := "/v1/chat/completions"
	if responses {
		endpoint = "/v1/responses"
	}
	if h.inspectPromptFilterOpenAI(c, body, endpoint, model) || h.enforceAPIKeyLimitsAndReply(c, model) {
		return true
	}
	stream := newPrismStream(c, r, responses)
	r.ResponseID = stream.id
	r.CreatedAt = stream.created
	// Use the same request budget for downstream writes and upstream work.
	requestTimeout := prismchannel.DefaultConfig().RequestTimeout
	if h.cfg != nil && h.cfg.Prism.RequestTimeout > 0 {
		requestTimeout = h.cfg.Prism.RequestTimeout
	}
	runCtx, cancelRun := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancelRun()
	stream.ctx = runCtx
	if r.Stream {
		if err := stream.checkWriteDeadline(); err != nil {
			prismError(c, http.StatusServiceUnavailable, "prism_stream_deadline_unsupported")
			return true
		}
	}
	release, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return true
	}
	if release != nil {
		defer release()
	}
	owner := responseCacheOwner(requestAPIKeyID(c))
	// Static keys without a DB ID still need separate context namespaces.
	if requestAPIKeyID(c) == 0 && c.GetString("apiKey") != "" {
		sum := sha256.Sum256([]byte(c.GetString("apiKey")))
		owner = fmt.Sprintf("static:%x", sum[:])
	}
	started := time.Now()
	firstMs := 0
	var streamMu sync.Mutex
	var keepaliveOnce sync.Once
	var keepaliveWG sync.WaitGroup
	keepaliveCtx, cancelKeepalive := context.WithCancel(runCtx)
	defer cancelKeepalive()
	keepaliveInterval := 10 * time.Second
	if h.cfg != nil && h.cfg.Prism.StreamKeepalive > 0 {
		keepaliveInterval = h.cfg.Prism.StreamKeepalive
	}
	var emit func(prismchannel.Event) error
	if r.Stream {
		emit = func(e prismchannel.Event) error {
			streamMu.Lock()
			defer streamMu.Unlock()
			keepaliveOnce.Do(func() {
				keepaliveWG.Add(1)
				go func() {
					defer keepaliveWG.Done()
					ticker := time.NewTicker(keepaliveInterval)
					defer ticker.Stop()
					for {
						select {
						case <-keepaliveCtx.Done():
							return
						case <-ticker.C:
							if err := emit(prismchannel.Event{Keepalive: true}); err != nil {
								cancelRun()
								return
							}
						}
					}
				}()
			})
			if e.Text != "" && firstMs == 0 {
				firstMs = int(time.Since(started).Milliseconds())
			}
			err := stream.Emit(e)
			if err != nil {
				cancelRun()
			}
			return err
		}
	}
	result, err := h.prismClient.Run(runCtx, owner, r, emit)
	cancelKeepalive()
	keepaliveWG.Wait()
	status := http.StatusOK
	if err != nil {
		status = prismErrorStatus(err)
		if errors.Is(err, prismchannel.ErrBusy) {
			c.Header("Retry-After", "1")
		}
		var ae *prismchannel.Error
		if errors.As(err, &ae) && ae.Status == 429 {
			c.Header("Retry-After", "1")
		}
		if runCtx.Err() == nil {
			if stream.started {
				stream.Fail(err.Error())
			} else {
				prismError(c, status, err.Error())
			}
		}
	} else {
		if r.Stream && firstMs == 0 && len(result.Calls) > 0 {
			firstMs = int(time.Since(started).Milliseconds())
		}
		if r.Stream {
			if err = stream.Finish(result); err != nil {
				status = 499
			}
		} else if responses {
			c.JSON(200, prismResponse(result, model, stream.messageID, "completed"))
		} else {
			c.JSON(200, prismChat(result, model, stream.created))
		}
	}
	if h.db != nil {
		h.logUsageForRequest(c, &database.UsageLogInput{RequestID: uuid.NewString(), Channel: "prism", Endpoint: endpoint, InboundEndpoint: endpoint, UpstreamEndpoint: prismchannel.PathStart, Model: model, EffectiveModel: model, StatusCode: status, Stream: r.Stream, InputTokens: int(result.Usage.InputTokens), OutputTokens: int(result.Usage.OutputTokens), PromptTokens: int(result.Usage.InputTokens), CompletionTokens: int(result.Usage.OutputTokens), CachedTokens: int(result.Usage.InputTokensDetails.CachedTokens), TotalTokens: int(result.Usage.TotalTokens), DurationMs: int(time.Since(started).Milliseconds()), FirstTokenMs: firstMs})
	}
	return true
}
func prismErrorStatus(e error) int {
	if errors.Is(e, prismchannel.ErrBusy) {
		return 429
	}
	if errors.Is(e, context.DeadlineExceeded) {
		return 504
	}
	if errors.Is(e, context.Canceled) {
		return 499
	}
	var ae *prismchannel.Error
	if errors.As(e, &ae) {
		if ae.Status == 400 || ae.Status == 413 || ae.Status == 429 {
			return ae.Status
		}
	}
	return 502
}
func prismError(c *gin.Context, status int, code string) {
	c.JSON(status, gin.H{"error": gin.H{"message": code, "type": "prism_error", "code": code}})
}

func (h *Handler) prismHealth(c *gin.Context) {
	if h.prismClient == nil {
		status := "disabled"
		httpStatus := 200
		if h.cfg != nil && h.cfg.Prism.Enabled {
			status = "unavailable"
			httpStatus = 503
		}
		c.JSON(httpStatus, gin.H{"status": status, "channel": "prism"})
		return
	}
	m := h.prismClient.Metrics()
	c.JSON(200, gin.H{"status": "configured", "channel": "prism", "capacity": h.prismClient.Capacity(), "inflight": m.Inflight.Load(), "queued": m.Queued.Load(), "paced_waiters": m.PacedWaiters.Load(), "warm_slots": m.WarmSlots.Load(), "streaming": "polling_sse", "live_verified": m.LiveVerifiedAt.Load() > 0, "last_live_success_at": m.LiveVerifiedAt.Load()})
}
func (h *Handler) prismMetrics(c *gin.Context) {
	if h.prismClient == nil {
		prismError(c, 503, "prism_unconfigured")
		return
	}
	c.Header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	h.prismClient.Metrics().Write(c.Writer, h.prismClient.Cache())
}
