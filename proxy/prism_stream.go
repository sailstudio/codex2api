package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/internal/prismchannel"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func prismUsage(r prismchannel.Result, responses bool) gin.H {
	if responses {
		return gin.H{"input_tokens": r.Usage.InputTokens, "output_tokens": r.Usage.OutputTokens, "total_tokens": r.Usage.TotalTokens, "input_tokens_details": gin.H{"cached_tokens": r.Usage.InputTokensDetails.CachedTokens}, "output_tokens_details": gin.H{"reasoning_tokens": 0}, "prism_cache_read_tokens": r.CacheReadTokens, "prism_cache_write_tokens": r.CacheWriteTokens, "prism_cache_estimated": true, "prism_estimated": r.Estimated}
	}
	return gin.H{"prompt_tokens": r.Usage.InputTokens, "completion_tokens": r.Usage.OutputTokens, "total_tokens": r.Usage.TotalTokens, "prompt_tokens_details": gin.H{"cached_tokens": r.Usage.InputTokensDetails.CachedTokens}, "prism_cache_read_tokens": r.CacheReadTokens, "prism_cache_write_tokens": r.CacheWriteTokens, "prism_cache_estimated": true, "prism_estimated": r.Estimated}
}
func prismCalls(calls []prismchannel.Item) []gin.H {
	out := make([]gin.H, 0, len(calls))
	for _, v := range calls {
		out = append(out, gin.H{"id": v.CallID, "type": "function", "function": gin.H{"name": v.Name, "arguments": v.Arguments}})
	}
	return out
}
func prismChat(r prismchannel.Result, model string, created int64) gin.H {
	m := gin.H{"role": "assistant", "content": r.Text}
	finish := "stop"
	if len(r.Calls) > 0 {
		m["tool_calls"] = prismCalls(r.Calls)
		finish = "tool_calls"
		if r.Text == "" {
			m["content"] = nil
		}
	}
	return gin.H{"id": r.ID, "object": "chat.completion", "created": created, "model": model, "choices": []gin.H{{"index": 0, "message": m, "finish_reason": finish}}, "usage": prismUsage(r, false)}
}
func prismMessage(id, text, status string) gin.H {
	return gin.H{"id": id, "type": "message", "role": "assistant", "status": status, "content": []gin.H{{"type": "output_text", "text": text, "annotations": []any{}}}}
}
func prismFunction(v prismchannel.Item, status string) gin.H {
	return gin.H{"id": v.ID, "type": "function_call", "status": status, "call_id": v.CallID, "name": v.Name, "arguments": v.Arguments}
}
func prismResponse(r prismchannel.Result, model, messageID, status string) gin.H {
	output := make([]gin.H, 0, len(r.Calls)+1)
	if r.Text != "" {
		output = append(output, prismMessage(messageID, r.Text, status))
	}
	for _, v := range r.Calls {
		output = append(output, prismFunction(v, "completed"))
	}
	return gin.H{"id": r.ID, "object": "response", "created_at": r.CreatedAt, "status": status, "model": model, "output": output, "output_text": r.Text, "usage": prismUsage(r, true), "error": nil, "incomplete_details": nil}
}

type prismStream struct {
	c                                  *gin.Context
	ctx                                context.Context
	writeTimeout                       time.Duration
	r                                  prismchannel.Request
	responses, started, messageStarted bool
	id, messageID                      string
	created                            int64
	sequence                           int
	text                               strings.Builder
}

func newPrismStream(c *gin.Context, r prismchannel.Request, responses bool) *prismStream {
	return &prismStream{c: c, ctx: c.Request.Context(), writeTimeout: 10 * time.Second, r: r, responses: responses, id: "resp_prism_" + uuid.NewString(), messageID: "msg_" + uuid.NewString(), created: time.Now().Unix()}
}

func (s *prismStream) controller() *http.ResponseController {
	var w http.ResponseWriter = s.c.Writer
	// Gin's Flush discards errors. Use its wrapped writer so FlushError and
	// connection deadlines reach net/http, while writes still update Gin's state.
	if u, ok := w.(interface{ Unwrap() http.ResponseWriter }); ok {
		w = u.Unwrap()
	}
	return http.NewResponseController(w)
}

func (s *prismStream) checkWriteDeadline() error {
	return s.controller().SetWriteDeadline(time.Time{})
}

// All frames, including heartbeats and terminal markers, have a fresh write
// budget. Cancellation forces the in-progress socket operation to expire.
func (s *prismStream) writeFrame(frame string) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	rc := s.controller()
	deadline := time.Now().Add(s.writeTimeout)
	if d, ok := s.ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := rc.SetWriteDeadline(deadline); err != nil {
		return err
	}
	interrupted := make(chan struct{})
	stop := context.AfterFunc(s.ctx, func() {
		_ = rc.SetWriteDeadline(time.Now())
		close(interrupted)
	})
	defer func() {
		// Do not let an old cancellation callback race with a subsequent frame.
		if !stop() {
			<-interrupted
		}
		_ = rc.SetWriteDeadline(time.Time{})
	}()
	if _, err := s.c.Writer.Write([]byte(frame)); err != nil {
		return err
	}
	s.c.Writer.WriteHeaderNow()
	return rc.Flush()
}

func (s *prismStream) write(event string, v gin.H) error {
	if s.c.Request.Context().Err() != nil {
		return s.c.Request.Context().Err()
	}
	if s.responses {
		v["type"] = event
		v["sequence_number"] = s.sequence
		s.sequence++
	}
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	frame := fmt.Sprintf("data: %s\n\n", b)
	if event != "" && s.responses {
		frame = fmt.Sprintf("event: %s\n", event) + frame
	}
	return s.writeFrame(frame)
}
func (s *prismStream) chunk(delta gin.H, finish any) gin.H {
	return gin.H{"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.r.Model, "choices": []gin.H{{"index": 0, "delta": delta, "finish_reason": finish}}}
}
func (s *prismStream) begin() error {
	if s.started {
		return nil
	}
	s.started = true
	s.c.Header("Content-Type", "text/event-stream")
	s.c.Header("Cache-Control", "no-cache")
	s.c.Header("X-Accel-Buffering", "no")
	if s.responses {
		r := prismchannel.Result{ID: s.id, CreatedAt: s.created}
		if e := s.write("response.created", gin.H{"response": prismResponse(r, s.r.Model, s.messageID, "in_progress")}); e != nil {
			return e
		}
		return s.write("response.in_progress", gin.H{"response": prismResponse(r, s.r.Model, s.messageID, "in_progress")})
	}
	return s.write("", s.chunk(gin.H{"role": "assistant", "content": ""}, nil))
}
func (s *prismStream) Emit(e prismchannel.Event) error {
	if err := s.begin(); err != nil {
		return err
	}
	if e.Keepalive {
		return s.writeFrame(": prism keepalive\n\n")
	}
	if e.Text == "" {
		return nil
	}
	s.text.WriteString(e.Text)
	if !s.responses {
		return s.write("", s.chunk(gin.H{"content": e.Text}, nil))
	}
	if !s.messageStarted {
		s.messageStarted = true
		if err := s.write("response.output_item.added", gin.H{"output_index": 0, "item": gin.H{"id": s.messageID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}}); err != nil {
			return err
		}
		if err := s.write("response.content_part.added", gin.H{"item_id": s.messageID, "output_index": 0, "content_index": 0, "part": gin.H{"type": "output_text", "text": "", "annotations": []any{}}}); err != nil {
			return err
		}
	}
	return s.write("response.output_text.delta", gin.H{"item_id": s.messageID, "output_index": 0, "content_index": 0, "delta": e.Text})
}
func (s *prismStream) Finish(r prismchannel.Result) error {
	if err := s.begin(); err != nil {
		return err
	}
	r.ID = s.id
	if !s.responses {
		for i, v := range prismCalls(r.Calls) {
			v["index"] = i
			if err := s.write("", s.chunk(gin.H{"tool_calls": []gin.H{v}}, nil)); err != nil {
				return err
			}
		}
		finish := "stop"
		if len(r.Calls) > 0 {
			finish = "tool_calls"
		}
		if err := s.write("", s.chunk(gin.H{}, finish)); err != nil {
			return err
		}
		if s.r.IncludeUsage {
			if err := s.write("", gin.H{"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.r.Model, "choices": []any{}, "usage": prismUsage(r, false)}); err != nil {
				return err
			}
		}
		return s.writeFrame("data: [DONE]\n\n")
	}
	index := 0
	if s.messageStarted {
		if err := s.write("response.output_text.done", gin.H{"item_id": s.messageID, "output_index": 0, "content_index": 0, "text": r.Text}); err != nil {
			return err
		}
		if err := s.write("response.content_part.done", gin.H{"item_id": s.messageID, "output_index": 0, "content_index": 0, "part": gin.H{"type": "output_text", "text": r.Text, "annotations": []any{}}}); err != nil {
			return err
		}
		if err := s.write("response.output_item.done", gin.H{"output_index": 0, "item": prismMessage(s.messageID, r.Text, "completed")}); err != nil {
			return err
		}
		index = 1
	}
	for _, v := range r.Calls {
		if err := s.write("response.output_item.added", gin.H{"output_index": index, "item": prismFunction(prismchannel.Item{ID: v.ID, CallID: v.CallID, Name: v.Name}, "in_progress")}); err != nil {
			return err
		}
		if err := s.write("response.function_call_arguments.delta", gin.H{"item_id": v.ID, "output_index": index, "delta": v.Arguments}); err != nil {
			return err
		}
		if err := s.write("response.function_call_arguments.done", gin.H{"item_id": v.ID, "output_index": index, "arguments": v.Arguments}); err != nil {
			return err
		}
		if err := s.write("response.output_item.done", gin.H{"output_index": index, "item": prismFunction(v, "completed")}); err != nil {
			return err
		}
		index++
	}
	return s.write("response.completed", gin.H{"response": prismResponse(r, s.r.Model, s.messageID, "completed")})
}
func (s *prismStream) Fail(code string) {
	if s.responses {
		_ = s.write("response.failed", gin.H{"response": gin.H{"id": s.id, "object": "response", "status": "failed", "error": gin.H{"code": "prism_error", "message": code}}})
	} else {
		_ = s.write("", gin.H{"error": gin.H{"code": "prism_error", "message": code}})
		_ = s.writeFrame("data: [DONE]\n\n")
	}
}
