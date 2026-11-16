package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var maxChatBodyBytes int64 = 64 << 20

// handleChat 实现 POST /v1/chat/completions（SPEC §6.3）。
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	// 日志条目在函数开头创建，defer 收尾，保证所有返回路径都恰好落一条日志（SPEC-A §2）。
	entry := RequestLog{Time: nowRFC3339(), Method: http.MethodPost, Path: "/v1/chat/completions"}
	started := time.Now()
	defer func() {
		entry.DurationMs = time.Since(started).Milliseconds()
		s.logs.Add(entry)
		if s.cfg.Log.AccessLogEnabled() {
			s.logger.Infof("[access] %s %s model=%s client_model=%s account=%s status=%d stream=%v duration_ms=%d prompt=%d completion=%d reasoning=%d error=%s",
				entry.Method, entry.Path, entry.Model, entry.ClientModel, entry.Account, entry.Status, entry.Stream,
				entry.DurationMs, entry.PromptTokens, entry.CompletionTokens, entry.ReasoningTokens, entry.Error)
		}
	}()

	r.Body = http.MaxBytesReader(w, r.Body, maxChatBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			entry.Status = http.StatusRequestEntityTooLarge
			entry.Error = "请求体超过 64MB 上限"
			writeError(w, http.StatusRequestEntityTooLarge, apiError{
				Message: "请求体超过 64MB 上限",
				Type:    typeInvalidRequest,
				Code:    "request_too_large",
			})
			return
		}
		entry.Status = http.StatusBadRequest
		entry.Error = "读取请求体失败：" + err.Error()
		writeError(w, http.StatusBadRequest, apiError{
			Message: "读取请求体失败：" + err.Error(),
			Type:    typeInvalidRequest,
		})
		return
	}

	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &fields); err != nil {
		entry.Status = http.StatusBadRequest
		entry.Error = "请求体不是合法 JSON：" + err.Error()
		writeError(w, http.StatusBadRequest, apiError{
			Message: "请求体不是合法 JSON：" + err.Error(),
			Type:    typeInvalidRequest,
			Code:    "invalid_json",
		})
		return
	}

	// 模型解析：缺省回填默认模型，其余原样透传给上游（SPEC4 §F.2.3）。
	clientModel := ""
	if raw, ok := fields["model"]; ok {
		_ = json.Unmarshal(raw, &clientModel)
	}
	resolvedClient := clientModel
	if resolvedClient == "" {
		resolvedClient = s.cfg.Models.DefaultModelValue()
	}
	upstreamModel := resolvedClient

	// 模型校验（SPEC §6.3.3）。
	if s.cfg.Models.ValidateValue() == "catalog" {
		if s.catalog.Count() == 0 {
			s.logger.Warnf("[chat] 警告：模型目录为空，跳过模型校验（model=%s）", resolvedClient)
		} else if !s.catalog.Has(upstreamModel) {
			s.logger.Warnf("[chat] 未知模型 %s，拒绝请求（可用模型见 /v1/models）", resolvedClient)
			entry.Status = http.StatusBadRequest
			entry.Error = "未知模型 " + resolvedClient + "，可用模型见 /v1/models"
			entry.ClientModel = resolvedClient
			entry.Model = resolvedClient
			writeError(w, http.StatusBadRequest, apiError{
				Message: "未知模型 " + resolvedClient + "，可用模型见 /v1/models",
				Type:    typeInvalidRequest,
				Code:    "model_not_found",
			})
			return
		}
	}

	stream := false
	if raw, ok := fields["stream"]; ok {
		_ = json.Unmarshal(raw, &stream)
	}
	entry.Model = upstreamModel
	entry.ClientModel = resolvedClient
	entry.Stream = stream

	// 只替换 model 键，其余字段原样保留（SPEC §6.3.4）。
	modelJSON, err := json.Marshal(upstreamModel)
	if err != nil {
		writeError(w, http.StatusInternalServerError, apiError{Message: "序列化模型名失败", Type: typeInvalidRequest})
		return
	}
	fields["model"] = json.RawMessage(modelJSON)
	upstreamBody, err := json.Marshal(fields)
	if err != nil {
		writeError(w, http.StatusInternalServerError, apiError{Message: "序列化请求体失败", Type: typeInvalidRequest})
		return
	}

	// 请求级总超时（0 = 不设，长回答流式）。
	ctx := r.Context()
	if timeout := s.cfg.Upstream.RequestTimeout(); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	maxAttempts := s.cfg.Accounts.MaxAttemptsValue()
	tried := map[string]bool{}
	lastStatus := 0
	var lastErr apiError
	attempted := false
	rateRetries := 0
	const maxRateRetries = 2

	for attempt := 0; attempt < maxAttempts; attempt++ {
		account := s.pool.Pick(tried)
		if account == nil {
			break
		}
		attempted = true
		tried[account.ID] = true
		entry.Account = account.ID
		// 该「账号+模型」组合仍在冷却中就直接跳过：只挡这一个模型，
		// 不影响同一账号的其它模型（否则一个热门模型被限流，整台机器都不可用）。
		if rem := s.modelCooldownRemaining(account.ID, upstreamModel); rem > 0 {
			lastStatus = http.StatusTooManyRequests
			lastErr = apiError{
				Message: "模型 " + upstreamModel + " 在该账号上被上游限流，约 " + strconv.Itoa(int(rem.Seconds()+0.5)) + " 秒后可重试；换用其它模型不受影响",
				Type:    typeRateLimit,
				Code:    "model_rate_limited",
			}
			s.logger.Warnf("[chat] 跳过账号 %s：模型 %s 仍在冷却（剩余 %s）", account.ID, upstreamModel, rem.Round(time.Second))
			continue
		}
		s.logger.Debugf("[chat] 选到账号 %s（第 %d 次尝试，model=%s）", account.ID, attempt+1, upstreamModel)

		token, err := s.pool.Token(ctx, account.ID, false)
		if err != nil {
			s.logger.Warnf("[chat] 账号 %s 刷新失败：%v", account.ID, err)
			lastErr = apiError{Message: "账号 " + account.ID + " 刷新失败：" + err.Error(), Type: typeUpstream, Code: "token_refresh_failed"}
			lastStatus = http.StatusBadGateway
			continue
		}
		// 并发闸门：超出 accounts.max_concurrent 的请求在这里排队，
		// 避免瞬时并发触发上游 429 user_model_rate_limited。
		if !s.acquireSlot(ctx) {
			lastErr = apiError{Message: "客户端在排队等待上游并发槽时取消了请求", Type: typeUpstream, Code: "client_canceled"}
			lastStatus = http.StatusRequestTimeout
			break
		}
		resp, err := s.sendChat(ctx, token, upstreamBody, stream)
		if err != nil {
			s.releaseSlot()
			lastErr = apiError{Message: "转发上游失败：" + err.Error(), Type: typeUpstream, Code: "upstream_network"}
			lastStatus = http.StatusBadGateway
			continue
		}

		// 401 → 强制刷新同账号并重试一次（不计入 max_attempts，SPEC §5.3）。
		if resp.StatusCode == http.StatusUnauthorized && s.cfg.Accounts.RefreshOn401Value() {
			_ = resp.Body.Close()
			token, err = s.pool.Token(ctx, account.ID, true)
			if err != nil {
				s.releaseSlot()
				s.pool.MarkError(account.ID, err.Error())
				s.logger.Warnf("[chat] 账号 %s 强制刷新失败：%v", account.ID, err)
				lastErr = apiError{Message: "账号 " + account.ID + " 强制刷新失败：" + err.Error(), Type: typeUpstream, Code: "token_refresh_failed"}
				lastStatus = http.StatusBadGateway
				continue
			}
			resp, err = s.sendChat(ctx, token, upstreamBody, stream)
			if err != nil {
				s.releaseSlot()
				lastErr = apiError{Message: "转发上游失败：" + err.Error(), Type: typeUpstream, Code: "upstream_network"}
				lastStatus = http.StatusBadGateway
				continue
			}
		}

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			if stream {
				s.forwardStream(ctx, w, resp, &entry, resolvedClient)
			} else {
				s.forwardJSON(w, resp, &entry, resolvedClient)
			}
			s.releaseSlot()
			return
		case resp.StatusCode == http.StatusUnauthorized:
			errBody := readErrorBody(resp)
			s.releaseSlot()
			s.pool.MarkError(account.ID, "上游返回 401")
			lastErr = buildUpstreamError(http.StatusUnauthorized, errBody)
			lastStatus = http.StatusBadGateway
			continue
		case resp.StatusCode == http.StatusTooManyRequests:
			// 429 有两种含义，处理方式不同：
			//   1) 上游频率限流（user_model_rate_limited）：瞬时性的，先短退避重试，不要一上来就把唯一账号封 10 分钟；
			//   2) 其它 429（含额度类）：重试无意义，冷却账号并换号。
			errBody := readErrorBody(resp)
			upErr := buildUpstreamError(http.StatusTooManyRequests, errBody)
			inUse, capacity := s.slotUsage()
			s.logger.Warnf("[chat] 账号 %s 收到 429（message=%q body=%s），在途并发 %d/%d",
				account.ID, upErr.Message, truncateForLog(errBody, 160), inUse, capacity)
			lastErr = upErr
			lastStatus = http.StatusTooManyRequests
			s.releaseSlot()
			// 上游 429 分两类，绝不能一律封账号：
			//   1) user_model_rate_limited —— 按「账号+模型」限流，只冷却这一个模型；
			//   2) 其它 429（额度类等）—— 短退避重试，仍失败才冷却整个账号。
			if isModelScopedRateLimit(errBody) {
				cd := s.setModelCooldown(account.ID, upstreamModel)
				upErr.Message = "模型 " + upstreamModel + " 被上游限流（" + upErr.Message + "），该模型冷却 " + strconv.Itoa(int(cd.Seconds())) + " 秒；其它模型不受影响"
				lastErr = upErr
				s.logger.Warnf("[chat] 模型 %s 在账号 %s 上被限流，仅冷却该模型 %s", upstreamModel, account.ID, cd)
				continue
			}
			if rateRetries < maxRateRetries && ctx.Err() == nil {
				rateRetries++
				backoff := time.Duration(300*(1<<(rateRetries-1)) * time.Millisecond)
				s.logger.Warnf("[chat] 429 短退避后重试（第 %d/%d 次，等待 %s）", rateRetries, maxRateRetries, backoff)
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
				}
				attempt-- // 这次短退避重试不占用换号次数
				continue
			}
			s.pool.Cooldown(account.ID)
			s.logger.Warnf("[chat] 账号 %s 连续 429，进入冷却 %s", account.ID, s.cfg.Accounts.Cooldown())
			continue
		case resp.StatusCode >= 500:
			errBody := readErrorBody(resp)
			s.releaseSlot()
			lastErr = buildUpstreamError(resp.StatusCode, errBody)
			lastStatus = http.StatusBadGateway
			continue
		default:
			// 其它 4xx：不换号，原样透传状态码与上游 message（SPEC §5.4）。
			errBody := readErrorBody(resp)
			s.releaseSlot()
			upErr := buildUpstreamError(resp.StatusCode, errBody)
			entry.Status = resp.StatusCode
			entry.Error = upErr.Message
			entry.UpstreamStatus = upErr.UpstreamStatus
			entry.UpstreamCode = upErr.UpstreamCode
			writeError(w, resp.StatusCode, upErr)
			return
		}
	}

	if !attempted {
		entry.Status = http.StatusServiceUnavailable
		entry.Error = "no_account"
		writeError(w, http.StatusServiceUnavailable, apiError{
			Message: "没有可用账号：请在 data/accounts.json 里配置小浣熊账号",
			Type:    typeNoAccount,
			Code:    "no_account",
		})
		return
	}

	if lastStatus == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", strconv.Itoa(int(s.cfg.Accounts.Cooldown().Seconds())))
		entry.Status = http.StatusTooManyRequests
		entry.Error = lastErr.Message
		entry.UpstreamStatus = lastErr.UpstreamStatus
		entry.UpstreamCode = lastErr.UpstreamCode
		writeError(w, http.StatusTooManyRequests, lastErr)
		return
	}
	if lastErr.Message == "" {
		lastErr = apiError{Message: "上游请求失败", Type: typeUpstream, Code: "upstream_error"}
	}
	entry.Status = http.StatusBadGateway
	entry.Error = lastErr.Message
	entry.UpstreamStatus = lastErr.UpstreamStatus
	entry.UpstreamCode = lastErr.UpstreamCode
	writeError(w, http.StatusBadGateway, lastErr)
}

// sendChat 转发一次聊天请求。
func (s *Server) sendChat(ctx context.Context, token string, body []byte, stream bool) (*http.Response, error) {
	if stream {
		return s.client.ChatStream(ctx, token, body)
	}
	return s.client.ChatJSON(ctx, token, body)
}

// readErrorBody 读取上游错误体（限 64KB）。
func readErrorBody(resp *http.Response) []byte {
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return data
}

// truncateForLog 把错误体截成适合写进日志的一行（去换行、限长）。
func truncateForLog(body []byte, max int) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// buildUpstreamError 把上游错误体转成错误信封（含状态码与上游 message/code）。
func buildUpstreamError(status int, body []byte) apiError {
	message := "上游返回 " + strconv.Itoa(status)
	var payload struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
		Details string `json:"details"`
		// 上游 429 用的是 OpenAI 风格嵌套信封：
		// {"error":{"code":"user_model_rate_limited","message":"…","type":"rate_limit_error"}}
		Error struct {
			Code    json.RawMessage `json:"code"`
			Message string          `json:"message"`
			Type    string          `json:"type"`
		} `json:"error"`
	}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &payload)
	}
	detail := payload.Message
	if detail == "" {
		detail = payload.Error.Message
	}
	if detail == "" {
		detail = payload.Details
	}
	if detail != "" {
		message += "：" + detail
	}
	err := apiError{
		Message:        message,
		Type:           typeUpstream,
		Code:           "upstream_" + strconv.Itoa(status),
		UpstreamStatus: &status,
	}
	if status == http.StatusTooManyRequests {
		err.Type = typeRateLimit
	}
	if payload.Code != nil {
		err.UpstreamCode = payload.Code
	}
	return err
}

// isModelScopedRateLimit 判断 429 是否只针对某个模型。
// 上游报文形如 {"error":{"code":"user_model_rate_limited","message":"您的模型请求频率超限，请稍后重试"}}，
// code 里带 model 说明限制挂在「账号+模型」上，不应该把整个账号冷却。
func isModelScopedRateLimit(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var payload struct {
		Error struct {
			Code json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	code := strings.ToLower(strings.Trim(strings.TrimSpace(string(payload.Error.Code)), `"`))
	return strings.Contains(code, "model")
}

// usageInfo 对应上游的 usage 字段。
type usageInfo struct {
	PromptTokens            int64 `json:"prompt_tokens"`
	CompletionTokens        int64 `json:"completion_tokens"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func applyUsage(entry *RequestLog, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var usage usageInfo
	if err := json.Unmarshal(raw, &usage); err != nil {
		return
	}
	entry.PromptTokens = usage.PromptTokens
	entry.CompletionTokens = usage.CompletionTokens
	entry.ReasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
}

// forwardStream 逐行透传上游 SSE，并把每帧的 model 改写为客户端请求名。
func (s *Server) forwardStream(ctx context.Context, w http.ResponseWriter, resp *http.Response, entry *RequestLog, clientModel string) {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Xhx-Model", entry.Model)
	w.Header().Set("X-Xhx-Account", entry.Account)
	entry.Status = resp.StatusCode
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(resp.Body)
	frames := 0
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			if strings.HasPrefix(strings.TrimSpace(line), "data:") {
				frames++
			}
			s.writeSSELine(w, flusher, line, clientModel, entry)
		}
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				s.logger.Warnf("[chat] 上游流中断：%v", err)
				entry.Error = "上游流中断：" + err.Error()
			}
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	s.logger.Debugf("[chat] 流式结束：共 %d 个 data 帧（model=%s account=%s）", frames, clientModel, entry.Account)
}

// writeSSELine 处理并写出一行 SSE。
func (s *Server) writeSSELine(w io.Writer, flusher http.Flusher, line, clientModel string, entry *RequestLog) {
	trimmed := strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(trimmed, "data:") {
		_, _ = io.WriteString(w, line)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		_, _ = io.WriteString(w, line)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	frame := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		// 无法解析的 data: 行原样透传。
		_, _ = io.WriteString(w, line)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	if raw, ok := frame["usage"]; ok {
		applyUsage(entry, raw)
	}
	if modelJSON, err := json.Marshal(clientModel); err == nil {
		frame["model"] = json.RawMessage(modelJSON)
	}
	rewritten, err := json.Marshal(frame)
	if err != nil {
		_, _ = io.WriteString(w, line)
	} else {
		_, _ = io.WriteString(w, "data: "+string(rewritten)+"\n")
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// forwardJSON 改写非流式响应体的 model 后原样下发。
func (s *Server) forwardJSON(w http.ResponseWriter, resp *http.Response, entry *RequestLog, clientModel string) {
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxChatBodyBytes))
	if err != nil {
		entry.Status = http.StatusBadGateway
		entry.Error = "读取上游响应失败：" + err.Error()
		writeError(w, http.StatusBadGateway, apiError{Message: entry.Error, Type: typeUpstream, Code: "upstream_read_error"})
		return
	}
	entry.Status = resp.StatusCode
	w.Header().Set("X-Xhx-Model", entry.Model)
	w.Header().Set("X-Xhx-Account", entry.Account)

	frame := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &frame); err != nil {
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
		return
	}
	if raw, ok := frame["usage"]; ok {
		applyUsage(entry, raw)
	}
	if modelJSON, err := json.Marshal(clientModel); err == nil {
		frame["model"] = json.RawMessage(modelJSON)
	}
	rewritten, err := json.Marshal(frame)
	if err != nil {
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(rewritten)
}
