package server

// responses.go 实现 OpenAI Responses API（POST /v1/responses）。
//
// 设计取舍：不重写账号池选号、401 重试、429 冷却、错误映射等一整套 chat 逻辑，
// 而是把 Responses 请求「翻译」成一次内部 /v1/chat/completions 请求，再用一个
// 旁路 ResponseWriter（tap）截获内部处理器写给客户端的字节流：
//   - 流式：逐帧解析 chat 的 SSE delta，实时改写成 Responses 的 event 序列；
//   - 非流式：等内部处理器结束后一次性解析 chat JSON，包装成 response 对象。
// 这样 /v1/responses 与 /v1/chat/completions 的账号策略、限流行为完全一致。

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxResponsesBodyBytes int64 = 64 << 20

// randHexID 生成 n 字节的随机十六进制串（n 字节 → 2n 个字符）。
func randHexID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// ---------- 请求解析：Responses → chat ----------

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Function    *struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type responsesReq struct {
	Model             string          `json:"model"`
	Input             json.RawMessage `json:"input"`
	Instructions      string          `json:"instructions"`
	Stream            bool            `json:"stream"`
	MaxOutputTokens   *int            `json:"max_output_tokens"`
	Temperature       *float64        `json:"temperature"`
	TopP              *float64        `json:"top_p"`
	Tools             []responsesTool `json:"tools"`
	ToolChoice        json.RawMessage `json:"tool_choice"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
	User              string          `json:"user"`
}

// buildChatBody 把 Responses 请求体翻译成 chat/completions 请求体。
func buildChatBody(raw []byte, defaultModel string) (chatBody []byte, stream bool, model string, err error) {
	var req responsesReq
	if err2 := json.Unmarshal(raw, &req); err2 != nil {
		return nil, false, "", fmt.Errorf("请求体不是合法 JSON：%v", err2)
	}
	model = strings.TrimSpace(req.Model)
	if model == "" {
		model = defaultModel
	}
	stream = req.Stream

	chat := map[string]any{"model": model, "stream": stream}

	var msgs []any
	if strings.TrimSpace(req.Instructions) != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": req.Instructions})
	}
	inputMsgs, err2 := buildMessages(req.Input)
	if err2 != nil {
		return nil, false, "", err2
	}
	msgs = append(msgs, inputMsgs...)
	if len(msgs) == 0 {
		msgs = append(msgs, map[string]any{"role": "user", "content": ""})
	}
	chat["messages"] = msgs

	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		chat["max_tokens"] = *req.MaxOutputTokens
	}
	if req.Temperature != nil {
		chat["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		chat["top_p"] = *req.TopP
	}
	if req.User != "" {
		chat["user"] = req.User
	}

	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			name, desc, params := t.Name, t.Description, t.Parameters
			if t.Function != nil && t.Function.Name != "" {
				name, desc, params = t.Function.Name, t.Function.Description, t.Function.Parameters
			}
			if name == "" {
				continue
			}
			fn := map[string]any{"name": name}
			if desc != "" {
				fn["description"] = desc
			}
			if len(params) > 0 {
				fn["parameters"] = params
			} else {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		if len(tools) > 0 {
			chat["tools"] = tools
		}
	}
	if tc, ok := convertToolChoice(req.ToolChoice); ok {
		chat["tool_choice"] = tc
	}

	body, err2 := json.Marshal(chat)
	if err2 != nil {
		return nil, false, "", fmt.Errorf("序列化聊天请求失败：%v", err2)
	}
	return body, stream, model, nil
}

// buildMessages 把 Responses 的 input（字符串或 item 数组）翻译成 chat messages。
func buildMessages(input json.RawMessage) ([]any, error) {
	if len(input) == 0 {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(input, &s); err == nil {
		return []any{map[string]any{"role": "user", "content": s}}, nil
	}

	var items []map[string]json.RawMessage
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, fmt.Errorf("input 只支持字符串或 message/function_call 数组")
	}

	out := make([]any, 0, len(items))
	for _, it := range items {
		typ := rawString(it["type"])
		switch typ {
		case "function_call_output":
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": rawString(it["call_id"]),
				"content":      rawToString(it["output"]),
			})
		case "function_call":
			callID := rawString(it["call_id"])
			if callID == "" {
				callID = "call_" + randHexID(8)
			}
			out = append(out, map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{map[string]any{
					"id":   callID,
					"type": "function",
					"function": map[string]any{
						"name":      rawString(it["name"]),
						"arguments": rawString(it["arguments"]),
					},
				}},
			})
		default: // message / 缺省
			role := rawString(it["role"])
			if role == "" {
				role = "user"
			}
			if role == "developer" {
				role = "system"
			}
			content, cErr := convertContent(it["content"])
			if cErr != nil {
				return nil, cErr
			}
			out = append(out, map[string]any{"role": role, "content": content})
		}
	}
	return out, nil
}

// convertContent 把 Responses 的 content parts 翻译成 chat 的 content（字符串或数组）。
func convertContent(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil
	}

	var texts []string
	blocks := make([]any, 0, len(parts))
	allText := true
	for _, p := range parts {
		switch rawString(p["type"]) {
		case "input_text", "output_text", "text", "summary_text":
			t := rawString(p["text"])
			texts = append(texts, t)
			blocks = append(blocks, map[string]any{"type": "text", "text": t})
		case "input_image":
			url := ""
			var su string
			if err := json.Unmarshal(p["image_url"], &su); err == nil {
				url = su
			} else {
				var o struct {
					URL string `json:"url"`
				}
				if err := json.Unmarshal(p["image_url"], &o); err == nil {
					url = o.URL
				}
			}
			if url == "" {
				continue
			}
			allText = false
			blocks = append(blocks, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		case "input_file":
			// 上游「上传文件」入口未逆向，这里只能丢弃并保留其余文字部分
			allText = false
		}
	}
	if allText {
		return strings.Join(texts, "\n"), nil
	}
	return blocks, nil
}

// convertToolChoice 把 Responses 的 tool_choice 翻译成 chat 的 tool_choice。
func convertToolChoice(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto", "none", "required":
			return s, true
		}
		return nil, false
	}
	var o struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, false
	}
	name := o.Name
	if name == "" && o.Function != nil {
		name = o.Function.Name
	}
	if name == "" {
		return nil, false
	}
	return map[string]any{"type": "function", "function": map[string]any{"name": name}}, true
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

func rawToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// ---------- 内部请求桥接用的 ResponseWriter ----------

// captureWriter 收集内部 chat 处理器的输出（非流式用）。
type captureWriter struct {
	hdr  http.Header
	code int
	buf  bytes.Buffer
}

func (c *captureWriter) Header() http.Header {
	if c.hdr == nil {
		c.hdr = http.Header{}
	}
	return c.hdr
}

func (c *captureWriter) WriteHeader(code int) {
	if c.code == 0 {
		c.code = code
	}
}

func (c *captureWriter) Write(p []byte) (int, error) { return c.buf.Write(p) }
func (c *captureWriter) Flush()                      {}

// tapWriter 把内部 chat 处理器的 SSE 行逐行转交给回调（流式用）。
type tapWriter struct {
	hdr    http.Header
	code   int
	raw    bytes.Buffer
	onLine func(string)
}

func (t *tapWriter) Header() http.Header {
	if t.hdr == nil {
		t.hdr = http.Header{}
	}
	return t.hdr
}

func (t *tapWriter) WriteHeader(code int) {
	if t.code == 0 {
		t.code = code
	}
}

func (t *tapWriter) Write(p []byte) (int, error) {
	t.raw.Write(p)
	if t.onLine != nil {
		t.onLine(string(p))
	}
	return len(p), nil
}

func (t *tapWriter) Flush() {}

// ---------- 端点 ----------

// handleResponses 实现 POST /v1/responses。
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	model := ""
	stream := false
	defer func() {
		s.logger.Infof("[access] POST /v1/responses model=%s stream=%v duration_ms=%d",
			model, stream, time.Since(started).Milliseconds())
	}()

	r.Body = http.MaxBytesReader(w, r.Body, maxResponsesBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, apiError{Message: "请求体超过 64MB 上限", Type: typeInvalidRequest, Code: "request_too_large"})
			return
		}
		writeError(w, http.StatusBadRequest, apiError{Message: "读取请求体失败：" + err.Error(), Type: typeInvalidRequest, Code: "invalid_body"})
		return
	}

	chatBody, isStream, resolved, bErr := buildChatBody(raw, s.cfg.Models.DefaultModelValue())
	if bErr != nil {
		writeError(w, http.StatusBadRequest, apiError{Message: bErr.Error(), Type: typeInvalidRequest, Code: "invalid_request"})
		return
	}
	model, stream = resolved, isStream

	// 构造内部 chat 请求：鉴权已由路由中间件完成，这里不再重复校验。
	inner, iErr := http.NewRequestWithContext(r.Context(), http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatBody))
	if iErr != nil {
		writeError(w, http.StatusInternalServerError, apiError{Message: "构造内部请求失败", Type: typeInvalidRequest, Code: "internal_error"})
		return
	}
	inner.Header = r.Header.Clone()
	inner.Header.Set("Content-Type", "application/json")

	respID := "resp_" + randHexID(12)
	createdAt := time.Now().Unix()

	if stream {
		s.streamResponses(w, inner, respID, model, createdAt)
		return
	}
	s.jsonResponses(w, inner, respID, model, createdAt)
}

// handleResponsesObject 实现 GET/DELETE /v1/responses/{id} 的空实现。
// 本反代不落库（无 store 能力），因此一律 404，并把语义讲清楚。
func (s *Server) handleResponsesObject(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, apiError{
		Message: "本反代不保存 response 对象（不支持 store/retrieve/background），请改用 stream:true 或直接取响应体",
		Type:    typeInvalidRequest,
		Code:    "not_found",
	})
}

// ---------- 非流式 ----------

func (s *Server) jsonResponses(w http.ResponseWriter, inner *http.Request, respID, model string, createdAt int64) {
	cw := &captureWriter{}
	s.handleChat(cw, inner)

	if cw.code >= 400 {
		// 内部处理器已写出统一的 OpenAI 错误信封，直接透传状态码与报文。
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(cw.code)
		_, _ = w.Write(cw.buf.Bytes())
		return
	}

	var resp struct {
		Choices []struct {
			Message struct {
				Content          string         `json:"content"`
				ReasoningContent string         `json:"reasoning_content"`
				ToolCalls        []chatToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens            int64 `json:"prompt_tokens"`
			CompletionTokens        int64 `json:"completion_tokens"`
			TotalTokens             int64 `json:"total_tokens"`
			CompletionTokensDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(cw.buf.Bytes(), &resp); err != nil {
		// 解析不了就把原始体回退给客户端，避免吞掉上游信息。
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write(cw.buf.Bytes())
		return
	}

	output := make([]any, 0, 1+len(resp.Choices))
	var text, reasoning string
	var toolCalls []chatToolCall
	if len(resp.Choices) > 0 {
		text = resp.Choices[0].Message.Content
		reasoning = resp.Choices[0].Message.ReasoningContent
		toolCalls = resp.Choices[0].Message.ToolCalls
	}

	output = append(output, map[string]any{
		"id":     "msg_" + randHexID(12),
		"type":   "message",
		"status": "completed",
		"role":   "assistant",
		"content": []any{map[string]any{
			"type":        "output_text",
			"text":        text,
			"annotations": []any{},
		}},
	})
	for _, tc := range toolCalls {
		callID := tc.ID
		if callID == "" {
			callID = "call_" + randHexID(10)
		}
		output = append(output, map[string]any{
			"id":        "fc_" + randHexID(10),
			"type":      "function_call",
			"status":    "completed",
			"call_id":   callID,
			"name":      tc.Function.Name,
			"arguments": tc.Function.Arguments,
		})
	}

	u := resp.Usage
	payload := map[string]any{
		"id":                 respID,
		"object":             "response",
		"created_at":         createdAt,
		"status":             "completed",
		"model":              model,
		"output":             output,
		"output_text":        text,
		"reasoning_effort":   nil,
		"parallel_tool_calls": true,
		"tool_choice":        "auto",
		"tools":              []any{},
		"usage": map[string]any{
			"input_tokens":  u.PromptTokens,
			"output_tokens": u.CompletionTokens,
			"total_tokens":  u.TotalTokens,
			"input_tokens_details": map[string]any{
				"cached_tokens": 0,
			},
			"output_tokens_details": map[string]any{
				"reasoning_tokens": u.CompletionTokensDetails.ReasoningTokens,
			},
		},
	}
	if reasoning != "" {
		// 上游思维链没有对应的 Responses 等价字段，放在私有扩展里避免客户端误解。
		payload["xhx_reasoning_content"] = reasoning
	}

	w.Header().Set("X-Xhx-Model", model)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(payload)
}

// ---------- 流式 ----------

// chatToolCall 是 chat 流里的 tool_calls 条目。
type chatToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatStreamFrame 是 chat 流里单个 SSE data 帧的结构。
type chatStreamFrame struct {
	Choices []struct {
		Delta struct {
			Content          string         `json:"content"`
			ReasoningContent string         `json:"reasoning_content"`
			ToolCalls        []chatToolCall `json:"tool_calls"`
		} `json:"delta"`
		Message struct {
			Content   string         `json:"content"`
			ToolCalls []chatToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens            int64 `json:"prompt_tokens"`
		CompletionTokens        int64 `json:"completion_tokens"`
		TotalTokens             int64 `json:"total_tokens"`
		CompletionTokensDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

// toolCallState 记录一个正在流式拼接的 function call。
type toolCallState struct {
	itemID      string
	callID      string
	name        string
	args        strings.Builder
	outputIndex int
}

// responsesStream 把 chat 的 SSE 增量翻译成 Responses 的事件序列。
type responsesStream struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	respID    string
	model     string
	createdAt int64

	headed    bool
	started   bool
	msgAdded  bool
	closed    bool
	itemID    string
	text      strings.Builder
	nextIndex int
	tools     map[int]*toolCallState
	toolOrder []int

	in, out, total, reason int64
}

func newResponsesStream(w http.ResponseWriter, respID, model string, createdAt int64) *responsesStream {
	flusher, _ := w.(http.Flusher)
	return &responsesStream{
		w:         w,
		flusher:   flusher,
		respID:    respID,
		model:     model,
		createdAt: createdAt,
		itemID:    "msg_" + randHexID(12),
		nextIndex: 1, // 0 号索引预留给 message
		tools:     map[int]*toolCallState{},
	}
}

func (c *responsesStream) ensureHeaders() {
	if c.headed {
		return
	}
	c.headed = true
	h := c.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	h.Set("X-Xhx-Model", c.model)
	c.w.WriteHeader(http.StatusOK)
}

func (c *responsesStream) emit(event string, payload map[string]any) {
	c.ensureHeaders()
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(c.w, "event: %s\ndata: %s\n\n", event, string(b))
	if c.flusher != nil {
		c.flusher.Flush()
	}
}

func (c *responsesStream) responseObject(status string, output []any) map[string]any {
	obj := map[string]any{
		"id":                  c.respID,
		"object":              "response",
		"created_at":          c.createdAt,
		"status":              status,
		"model":               c.model,
		"output":              output,
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"tools":               []any{},
	}
	if status == "completed" {
		obj["output_text"] = c.text.String()
		obj["usage"] = map[string]any{
			"input_tokens":  c.in,
			"output_tokens": c.out,
			"total_tokens":  c.total,
			"input_tokens_details": map[string]any{
				"cached_tokens": 0,
			},
			"output_tokens_details": map[string]any{
				"reasoning_tokens": c.reason,
			},
		}
	}
	return obj
}

func (c *responsesStream) messageItem(status string) map[string]any {
	content := []any{}
	if status == "completed" {
		content = []any{map[string]any{
			"type":        "output_text",
			"text":        c.text.String(),
			"annotations": []any{},
		}}
	}
	return map[string]any{
		"id":      c.itemID,
		"type":    "message",
		"status":  status,
		"role":    "assistant",
		"content": content,
	}
}

func (c *responsesStream) start() {
	if c.started {
		return
	}
	c.started = true
	resp := c.responseObject("in_progress", []any{})
	c.emit("response.created", map[string]any{"type": "response.created", "response": resp})
	c.emit("response.in_progress", map[string]any{"type": "response.in_progress", "response": resp})
}

func (c *responsesStream) ensureMessagePart() {
	if c.msgAdded {
		return
	}
	c.msgAdded = true
	c.emit("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": 0,
		"item":         c.messageItem("in_progress"),
	})
	c.emit("response.content_part.added", map[string]any{
		"type":          "response.content_part.added",
		"item_id":       c.itemID,
		"output_index":  0,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

func (c *responsesStream) appendText(delta string) {
	if delta == "" {
		return
	}
	c.ensureMessagePart()
	c.text.WriteString(delta)
	c.emit("response.output_text.delta", map[string]any{
		"type":          "response.output_text.delta",
		"item_id":       c.itemID,
		"output_index":  0,
		"content_index": 0,
		"delta":         delta,
	})
}

func (c *responsesStream) appendTool(index int, id, name, args string) {
	st, ok := c.tools[index]
	if !ok {
		st = &toolCallState{
			itemID:      "fc_" + randHexID(10),
			callID:      id,
			outputIndex: c.nextIndex,
		}
		if st.callID == "" {
			st.callID = "call_" + randHexID(10)
		}
		c.nextIndex++
		c.tools[index] = st
		c.toolOrder = append(c.toolOrder, index)
		c.emit("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": st.outputIndex,
			"item": map[string]any{
				"id":        st.itemID,
				"type":      "function_call",
				"status":    "in_progress",
				"call_id":   st.callID,
				"name":      name,
				"arguments": "",
			},
		})
	}
	if name != "" && st.name == "" {
		st.name = name
	}
	if args != "" {
		st.args.WriteString(args)
		c.emit("response.function_call_arguments.delta", map[string]any{
			"type":         "response.function_call_arguments.delta",
			"item_id":      st.itemID,
			"output_index": st.outputIndex,
			"delta":        args,
		})
	}
}

// onLine 处理内部 chat 处理器写出的每一行（只关心 data: 帧）。
func (c *responsesStream) onLine(line string) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	var frame chatStreamFrame
	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		return
	}

	// 收到第一帧才真正开始（这样上游直接报错时不会先发出 200 事件流）。
	c.start()

	if frame.Usage != nil {
		c.in = frame.Usage.PromptTokens
		c.out = frame.Usage.CompletionTokens
		c.total = frame.Usage.TotalTokens
		c.reason = frame.Usage.CompletionTokensDetails.ReasoningTokens
	}
	if len(frame.Choices) == 0 {
		return
	}
	d := frame.Choices[0].Delta
	if d.Content != "" {
		c.appendText(d.Content)
	}
	for _, tc := range d.ToolCalls {
		c.appendTool(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
	}
	// 兜底：个别上游在最后一帧给 message 而不是 delta
	if d.Content == "" && len(d.ToolCalls) == 0 && frame.Choices[0].Message.Content != "" {
		c.appendText(frame.Choices[0].Message.Content)
		for _, tc := range frame.Choices[0].Message.ToolCalls {
			c.appendTool(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
		}
	}
}

// finish 收尾：补 done 事件与 response.completed。
func (c *responsesStream) finish() {
	if c.closed {
		return
	}
	c.closed = true
	c.start()

	output := make([]any, 0, 1+len(c.toolOrder))
	if c.msgAdded {
		text := c.text.String()
		c.emit("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": c.itemID,
			"output_index": 0, "content_index": 0, "text": text,
		})
		c.emit("response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": c.itemID,
			"output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		})
		c.emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 0, "item": c.messageItem("completed"),
		})
		output = append(output, c.messageItem("completed"))
	}
	for _, idx := range c.toolOrder {
		st := c.tools[idx]
		args := st.args.String()
		item := map[string]any{
			"id": st.itemID, "type": "function_call", "status": "completed",
			"call_id": st.callID, "name": st.name, "arguments": args,
		}
		c.emit("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": st.itemID,
			"output_index": st.outputIndex, "arguments": args,
		})
		c.emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": st.outputIndex, "item": item,
		})
		output = append(output, item)
	}
	c.emit("response.completed", map[string]any{
		"type":     "response.completed",
		"response": c.responseObject("completed", output),
	})
}

func (s *Server) streamResponses(w http.ResponseWriter, inner *http.Request, respID, model string, createdAt int64) {
	conv := newResponsesStream(w, respID, model, createdAt)
	tap := &tapWriter{onLine: conv.onLine}
	s.handleChat(tap, inner)

	if tap.code >= 400 && !conv.started {
		// 还没开始流式就被内部处理器拒绝（401/429/503 等），按普通 JSON 错误返回。
		status := tap.code
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write(tap.raw.Bytes())
		return
	}
	conv.finish()
}
