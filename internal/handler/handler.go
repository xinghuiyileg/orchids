package handler

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/client"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/keeper"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/logger"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/tiktoken"
)

type Handler struct {
	config        *config.Config
	client        UpstreamClient
	loadBalancer  *loadbalancer.LoadBalancer
	keeper        *keeper.AccountKeeper
	requestLogger *logger.RequestLogger
}

// 重试配置
const (
	MaxRetryCount  = 3                      // 最大重试次数
	BaseRetryDelay = 100 * time.Millisecond // 基础重试延迟
)

// generateRequestID 生成唯一请求 ID
func generateRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

type UpstreamClient interface {
	SendRequest(ctx context.Context, prompt string, chatHistory []interface{}, model string, onMessage func(client.SSEMessage), logger *debug.Logger) error
}

type ClaudeRequest struct {
	Model     string              `json:"model"`
	Messages  []prompt.Message    `json:"messages"`
	System    []prompt.SystemItem `json:"system"`
	Tools     []interface{}       `json:"tools"`
	Stream    bool                `json:"stream"`
	MaxTokens int                 `json:"max_tokens,omitempty"`
	Thinking  *struct {
		Type        string `json:"type"`
		BudgetToken int    `json:"budget_tokens"`
	} `json:"thinking,omitempty"`
}

func New(cfg *config.Config) *Handler {
	return &Handler{
		config: cfg,
		client: client.New(cfg),
	}
}

func NewWithLoadBalancer(cfg *config.Config, lb *loadbalancer.LoadBalancer) *Handler {
	return &Handler{
		config:       cfg,
		loadBalancer: lb,
	}
}

func NewWithLoadBalancerAndKeeper(cfg *config.Config, lb *loadbalancer.LoadBalancer, k *keeper.AccountKeeper) *Handler {
	return &Handler{
		config:       cfg,
		loadBalancer: lb,
		keeper:       k,
	}
}

func NewWithAll(cfg *config.Config, lb *loadbalancer.LoadBalancer, k *keeper.AccountKeeper, l *logger.RequestLogger) *Handler {
	return &Handler{
		config:        cfg,
		loadBalancer:  lb,
		keeper:        k,
		requestLogger: l,
	}
}

var ModelMapping = map[string]string{
	"claude-opus-4-5":            "claude-opus-4.5",
	"claude-opus-4-5-20251101":   "claude-opus-4.5",
	"claude-haiku-4-5":           "claude-haiku-4.5",
	"claude-haiku-4-5-20251001":  "claude-haiku-4.5",
	"claude-sonnet-4-5":          "claude-sonnet-4.5",
	"claude-sonnet-4-5-20250929": "claude-sonnet-4.5",
	"claude-sonnet-4":            "claude-sonnet-4",
	"claude-sonnet-4-20250514":   "claude-sonnet-4",
	"claude-3-7-sonnet-20250219": "claude-3.7-sonnet",
	"claude-3-5-sonnet-20241022": "claude-sonnet-4.5",
	"claude-3-5-sonnet-latest":   "claude-sonnet-4.5",
	"claude-3-5-haiku-20241022":  "claude-haiku-4.5",
	"claude-3-5-haiku-latest":    "claude-haiku-4.5",
	"claude-3-opus-20240229":     "claude-opus-4.5",
	"claude-3-opus-latest":       "claude-opus-4.5",
}

var AvailableModels = []string{
	"claude-opus-4-5", "claude-opus-4-5-20251101",
	"claude-haiku-4-5", "claude-haiku-4-5-20251001",
	"claude-sonnet-4-5", "claude-sonnet-4-5-20250929",
	"claude-sonnet-4", "claude-sonnet-4-20250514", "claude-3-7-sonnet-20250219",
	"claude-3-5-sonnet-20241022", "claude-3-5-sonnet-latest",
	"claude-3-5-haiku-20241022", "claude-3-5-haiku-latest",
	"claude-3-opus-20240229", "claude-3-opus-latest",
}

func mapModel(requestModel string) string {
	if mapped, ok := ModelMapping[requestModel]; ok {
		return mapped
	}
	lowerModel := strings.ToLower(requestModel)
	if strings.Contains(lowerModel, "opus") {
		return "claude-opus-4.5"
	}
	if strings.Contains(lowerModel, "haiku") {
		return "claude-haiku-4.5"
	}
	return "claude-sonnet-4.5"
}

// fixToolInput 修复工具输入中的类型问题
func fixToolInput(inputJSON string) string {
	if inputJSON == "" {
		return "{}"
	}

	var input map[string]interface{}
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return inputJSON
	}

	fixed := false
	for key, value := range input {
		if strVal, ok := value.(string); ok {
			strVal = strings.TrimSpace(strVal)

			if strVal == "true" {
				input[key] = true
				fixed = true
				continue
			} else if strVal == "false" {
				input[key] = false
				fixed = true
				continue
			}

			if num, err := strconv.ParseInt(strVal, 10, 64); err == nil {
				input[key] = num
				fixed = true
				continue
			}

			if fnum, err := strconv.ParseFloat(strVal, 64); err == nil {
				input[key] = fnum
				fixed = true
				continue
			}

			if (strings.HasPrefix(strVal, "[") && strings.HasSuffix(strVal, "]")) ||
				(strings.HasPrefix(strVal, "{") && strings.HasSuffix(strVal, "}")) {
				var parsed interface{}
				if err := json.Unmarshal([]byte(strVal), &parsed); err == nil {
					input[key] = parsed
					fixed = true
				}
			}
		}
	}

	if !fixed {
		return inputJSON
	}

	result, err := json.Marshal(input)
	if err != nil {
		return inputJSON
	}
	return string(result)
}

func (h *Handler) HandleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	data := make([]map[string]interface{}, len(AvailableModels))
	for i, id := range AvailableModels {
		data[i] = map[string]interface{}{
			"id":       id,
			"object":   "model",
			"created":  1700000000,
			"owned_by": "anthropic",
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}

type OpenAIMessage struct {
	Role       string      `json:"role"`
	Content    interface{} `json:"content"`
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type OpenAIRequest struct {
	Model    string          `json:"model"`
	Messages []OpenAIMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	Tools    []interface{}   `json:"tools,omitempty"`
}

func (h *Handler) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var openaiReq OpenAIRequest
	if err := json.NewDecoder(r.Body).Decode(&openaiReq); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"message": "Invalid request body",
				"type":    "invalid_request_error",
			},
		})
		return
	}

	claudeReq := convertOpenAIToClaude(openaiReq)

	body, _ := json.Marshal(claudeReq)
	proxyReq, _ := http.NewRequestWithContext(r.Context(), "POST", "/v1/messages", bytes.NewReader(body))
	proxyReq.Header.Set("Content-Type", "application/json")

	if openaiReq.Stream {
		h.HandleMessages(w, proxyReq)
		return
	}

	rw := &bufferResponseWriter{header: make(http.Header), body: &bytes.Buffer{}}
	h.HandleMessages(rw, proxyReq)

	var claudeResp map[string]interface{}
	if err := json.Unmarshal(rw.body.Bytes(), &claudeResp); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"message": "Failed to parse response",
				"type":    "internal_error",
			},
		})
		return
	}

	openaiResp := convertClaudeToOpenAI(claudeResp, openaiReq.Model)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(openaiResp)
}

type bufferResponseWriter struct {
	header     http.Header
	body       *bytes.Buffer
	statusCode int
}

func (rw *bufferResponseWriter) Header() http.Header {
	return rw.header
}

func (rw *bufferResponseWriter) Write(b []byte) (int, error) {
	return rw.body.Write(b)
}

func (rw *bufferResponseWriter) WriteHeader(code int) {
	rw.statusCode = code
}

func (rw *bufferResponseWriter) Flush() {}

func convertOpenAIToClaude(req OpenAIRequest) ClaudeRequest {
	var messages []prompt.Message
	var system []prompt.SystemItem

	for _, msg := range req.Messages {
		if msg.Role == "system" {
			text := ""
			switch c := msg.Content.(type) {
			case string:
				text = c
			}
			system = append(system, prompt.SystemItem{Type: "text", Text: text})
			continue
		}

		var content prompt.MessageContent
		switch c := msg.Content.(type) {
		case string:
			content = prompt.MessageContent{Text: c}
		case []interface{}:
			var blocks []prompt.ContentBlock
			for _, item := range c {
				if m, ok := item.(map[string]interface{}); ok {
					blockType, _ := m["type"].(string)
					if blockType == "text" {
						text, _ := m["text"].(string)
						blocks = append(blocks, prompt.ContentBlock{Type: "text", Text: text})
					}
				}
			}
			content = prompt.MessageContent{Blocks: blocks}
		}

		if msg.Role == "tool" {
			blocks := []prompt.ContentBlock{{
				Type:      "tool_result",
				ToolUseID: msg.ToolCallID,
				Content:   msg.Content,
			}}
			messages = append(messages, prompt.Message{Role: "user", Content: prompt.MessageContent{Blocks: blocks}})
			continue
		}

		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			var blocks []prompt.ContentBlock
			if content.Text != "" {
				blocks = append(blocks, prompt.ContentBlock{Type: "text", Text: content.Text})
			}
			for _, tc := range msg.ToolCalls {
				var input interface{}
				json.Unmarshal([]byte(tc.Function.Arguments), &input)
				blocks = append(blocks, prompt.ContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: input,
				})
			}
			messages = append(messages, prompt.Message{Role: "assistant", Content: prompt.MessageContent{Blocks: blocks}})
			continue
		}

		messages = append(messages, prompt.Message{Role: msg.Role, Content: content})
	}

	var tools []interface{}
	for _, t := range req.Tools {
		if tm, ok := t.(map[string]interface{}); ok {
			if fn, ok := tm["function"].(map[string]interface{}); ok {
				tools = append(tools, map[string]interface{}{
					"name":         fn["name"],
					"description":  fn["description"],
					"input_schema": fn["parameters"],
				})
			}
		}
	}

	return ClaudeRequest{
		Model:    req.Model,
		Messages: messages,
		System:   system,
		Stream:   req.Stream,
		Tools:    tools,
	}
}

func convertClaudeToOpenAI(resp map[string]interface{}, model string) map[string]interface{} {
	content, _ := resp["content"].([]interface{})
	usage, _ := resp["usage"].(map[string]interface{})
	stopReason, _ := resp["stop_reason"].(string)

	var textContent string
	var toolCalls []map[string]interface{}

	for _, block := range content {
		if b, ok := block.(map[string]interface{}); ok {
			blockType, _ := b["type"].(string)
			if blockType == "text" {
				text, _ := b["text"].(string)
				textContent += text
			} else if blockType == "tool_use" {
				id, _ := b["id"].(string)
				name, _ := b["name"].(string)
				input, _ := b["input"]
				args, _ := json.Marshal(input)
				toolCalls = append(toolCalls, map[string]interface{}{
					"id":   id,
					"type": "function",
					"function": map[string]interface{}{
						"name":      name,
						"arguments": string(args),
					},
				})
			}
		}
	}

	finishReason := "stop"
	if stopReason == "tool_use" {
		finishReason = "tool_calls"
	}

	message := map[string]interface{}{
		"role":    "assistant",
		"content": textContent,
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	promptTokens := 0
	completionTokens := 0
	if usage != nil {
		if v, ok := usage["input_tokens"].(float64); ok {
			promptTokens = int(v)
		}
		if v, ok := usage["output_tokens"].(float64); ok {
			completionTokens = int(v)
		}
	}

	return map[string]interface{}{
		"id":      resp["id"],
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": map[string]interface{}{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}
}

func (h *Handler) HandleMessages(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	requestID := generateRequestID()

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ClaudeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// 初始化调试日志
	logger := debug.New(h.config.DebugEnabled)
	defer logger.Close()

	// 1. 记录进入的 Claude 请求
	logger.LogIncomingRequest(req)

	log.Printf("[%s] 新请求进入 model=%s stream=%v", requestID, req.Model, req.Stream)

	// 选择账号
	var apiClient UpstreamClient
	var currentAccount *store.Account
	var failedAccountIDs []int64

	selectAccount := func() error {
		if h.loadBalancer != nil {
			account, err := h.loadBalancer.GetNextAccountExcluding(failedAccountIDs)
			if err != nil {
				if h.client != nil {
					apiClient = h.client
					currentAccount = nil
					log.Printf("[%s] 负载均衡无可用账号，使用默认配置", requestID)
					return nil
				}
				return err
			}
			log.Printf("[%s] 使用账号: %s (%s)", requestID, account.Name, account.Email)
			apiClient = client.NewFromAccount(account)
			currentAccount = account
			return nil
		} else if h.client != nil {
			apiClient = h.client
			currentAccount = nil
			return nil
		}
		return errors.New("no client configured")
	}

	if err := selectAccount(); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	messages := req.Messages
	if len(messages) > 50 {
		messages = prompt.SummarizeHistory(messages, 4000)
	}

	hasCacheControl := prompt.HasCacheControl(req.System)

	builtPrompt := prompt.BuildPromptV2(prompt.ClaudeAPIRequest{
		Model:     req.Model,
		Messages:  messages,
		System:    req.System,
		Tools:     req.Tools,
		Stream:    req.Stream,
		MaxTokens: req.MaxTokens,
	})

	logger.LogConvertedPrompt(builtPrompt)

	mappedModel := mapModel(req.Model)
	log.Printf("[%s] 模型映射: %s -> %s", requestID, req.Model, mappedModel)

	isStream := req.Stream
	var flusher http.Flusher
	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		streamFlusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming not supported", http.StatusInternalServerError)
			return
		}
		flusher = streamFlusher
	} else {
		w.Header().Set("Content-Type", "application/json")
	}

	msgID := fmt.Sprintf("msg_%d", time.Now().UnixMilli())
	blockIndex := -1
	var hasReturn bool
	var mu sync.Mutex
	var finalStopReason string
	toolBlocks := make(map[string]int)
	var responseText strings.Builder
	var contentBlocks []map[string]interface{}
	var currentTextIndex = -1

	inputTokens := tiktoken.EstimateTextTokens(builtPrompt)
	var outputTokens int
	var cacheCreationTokens, cacheReadTokens int
	var outputMu sync.Mutex

	if hasCacheControl {
		cacheReadTokens = inputTokens / 10
	}

	addOutputTokens := func(text string) {
		if text == "" {
			return
		}
		tokens := tiktoken.EstimateTextTokens(text)
		outputMu.Lock()
		outputTokens += tokens
		outputMu.Unlock()
	}

	writeSSE := func(event, data string) {
		if !isStream {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if hasReturn {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		flusher.Flush()
		logger.LogOutputSSE(event, data)
	}

	usage := map[string]int{"input_tokens": inputTokens, "output_tokens": 0}
	if hasCacheControl {
		usage["cache_creation_input_tokens"] = cacheCreationTokens
		usage["cache_read_input_tokens"] = cacheReadTokens
	}

	startData, _ := json.Marshal(map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":      msgID,
			"type":    "message",
			"role":    "assistant",
			"content": []interface{}{},
			"model":   req.Model,
			"usage":   usage,
		},
	})
	writeSSE("message_start", string(startData))

	done := make(chan struct{})
	var retryCount int

	// 完成响应的闭包需要移到 retryCount 声明之后
	finishResponse := func(stopReason string) {
		mu.Lock()
		if hasReturn {
			mu.Unlock()
			return
		}
		hasReturn = true
		finalStopReason = stopReason
		mu.Unlock()

		if isStream {
			deltaData, _ := json.Marshal(map[string]interface{}{
				"type":  "message_delta",
				"delta": map[string]string{"stop_reason": stopReason},
				"usage": map[string]int{"output_tokens": outputTokens},
			})
			writeSSE("message_delta", string(deltaData))

			stopData, _ := json.Marshal(map[string]string{"type": "message_stop"})
			writeSSE("message_stop", string(stopData))
		}

		// 6. 记录摘要
		logger.LogSummary(inputTokens, outputTokens, time.Since(startTime), stopReason)
		log.Printf("[%s] 请求完成: 输入=%d tokens, 输出=%d tokens, 耗时=%v, 重试=%d",
			requestID, inputTokens, outputTokens, time.Since(startTime), retryCount)

		// 记录到实时日志系统
		if h.requestLogger != nil {
			accountName := "default"
			var accountID int64
			if currentAccount != nil {
				accountName = currentAccount.Name
				accountID = currentAccount.ID
			}
			success := stopReason != "error"
			h.requestLogger.LogRequest(requestID, accountID, accountName,
				fmt.Sprintf("model=%s, input=%d, output=%d, retries=%d",
					req.Model, inputTokens, outputTokens, retryCount),
				time.Since(startTime).Milliseconds(), success)
		}
	}

	go func() {
		defer close(done)
		for {
			err := apiClient.SendRequest(r.Context(), builtPrompt, []interface{}{}, mappedModel, func(msg client.SSEMessage) {
				mu.Lock()
				if hasReturn {
					mu.Unlock()
					return
				}
				mu.Unlock()

				eventKey := msg.Type
				if msg.Type == "model" && msg.Event != nil {
					if evtType, ok := msg.Event["type"].(string); ok {
						eventKey = "model." + evtType
					}
				}

				switch eventKey {
				case "model.reasoning-start":
					mu.Lock()
					blockIndex++
					idx := blockIndex
					mu.Unlock()
					data, _ := json.Marshal(map[string]interface{}{
						"type":          "content_block_start",
						"index":         idx,
						"content_block": map[string]string{"type": "thinking", "thinking": ""},
					})
					writeSSE("content_block_start", string(data))

				case "model.reasoning-delta":
					mu.Lock()
					idx := blockIndex
					mu.Unlock()
					delta, _ := msg.Event["delta"].(string)
					if isStream {
						addOutputTokens(delta)
					}
					data, _ := json.Marshal(map[string]interface{}{
						"type":  "content_block_delta",
						"index": idx,
						"delta": map[string]string{"type": "thinking_delta", "thinking": delta},
					})
					writeSSE("content_block_delta", string(data))

				case "model.reasoning-end":
					mu.Lock()
					idx := blockIndex
					mu.Unlock()
					data, _ := json.Marshal(map[string]interface{}{
						"type":  "content_block_stop",
						"index": idx,
					})
					writeSSE("content_block_stop", string(data))

				case "model.text-start":
					mu.Lock()
					blockIndex++
					idx := blockIndex
					mu.Unlock()
					if !isStream {
						contentBlocks = append(contentBlocks, map[string]interface{}{
							"type": "text",
							"text": "",
						})
						currentTextIndex = len(contentBlocks) - 1
					}
					data, _ := json.Marshal(map[string]interface{}{
						"type":          "content_block_start",
						"index":         idx,
						"content_block": map[string]string{"type": "text", "text": ""},
					})
					writeSSE("content_block_start", string(data))

				case "model.text-delta":
					mu.Lock()
					idx := blockIndex
					mu.Unlock()
					delta, _ := msg.Event["delta"].(string)
					addOutputTokens(delta)
					if !isStream {
						responseText.WriteString(delta)
						if currentTextIndex >= 0 && currentTextIndex < len(contentBlocks) {
							if text, ok := contentBlocks[currentTextIndex]["text"].(string); ok {
								contentBlocks[currentTextIndex]["text"] = text + delta
							}
						}
					}
					data, _ := json.Marshal(map[string]interface{}{
						"type":  "content_block_delta",
						"index": idx,
						"delta": map[string]string{"type": "text_delta", "text": delta},
					})
					writeSSE("content_block_delta", string(data))

				case "model.text-end":
					mu.Lock()
					idx := blockIndex
					mu.Unlock()
					data, _ := json.Marshal(map[string]interface{}{
						"type":  "content_block_stop",
						"index": idx,
					})
					writeSSE("content_block_stop", string(data))

				case "model.tool-input-start":
					toolID, _ := msg.Event["id"].(string)
					toolName, _ := msg.Event["toolName"].(string)
					if toolID == "" || toolName == "" {
						return
					}
					mu.Lock()
					blockIndex++
					idx := blockIndex
					toolBlocks[toolID] = idx
					mu.Unlock()

				case "model.tool-input-delta":
					// 忽略，等待 tool-call

				case "model.tool-input-end":
					// 忽略，等待 tool-call

				case "model.tool-call":
					toolID, _ := msg.Event["toolCallId"].(string)
					toolName, _ := msg.Event["toolName"].(string)
					inputStr, _ := msg.Event["input"].(string)
					if toolID == "" {
						return
					}
					if !isStream {
						addOutputTokens(toolName)
						addOutputTokens(inputStr)
						fixedInput := fixToolInput(inputStr)
						var inputValue interface{}
						if err := json.Unmarshal([]byte(fixedInput), &inputValue); err != nil {
							inputValue = map[string]interface{}{}
						}
						contentBlocks = append(contentBlocks, map[string]interface{}{
							"type":  "tool_use",
							"id":    toolID,
							"name":  toolName,
							"input": inputValue,
						})
						return
					}

					mu.Lock()
					idx, exists := toolBlocks[toolID]
					mu.Unlock()
					if !exists {
						return
					}

					addOutputTokens(toolName)
					addOutputTokens(inputStr)
					fixedInput := fixToolInput(inputStr)

					// content_block_start
					startData, _ := json.Marshal(map[string]interface{}{
						"type":  "content_block_start",
						"index": idx,
						"content_block": map[string]interface{}{
							"type":  "tool_use",
							"id":    toolID,
							"name":  toolName,
							"input": map[string]interface{}{},
						},
					})
					writeSSE("content_block_start", string(startData))

					// content_block_delta
					deltaData, _ := json.Marshal(map[string]interface{}{
						"type":  "content_block_delta",
						"index": idx,
						"delta": map[string]interface{}{
							"type":         "input_json_delta",
							"partial_json": fixedInput,
						},
					})
					writeSSE("content_block_delta", string(deltaData))

					// content_block_stop
					stopData, _ := json.Marshal(map[string]interface{}{
						"type":  "content_block_stop",
						"index": idx,
					})
					writeSSE("content_block_stop", string(stopData))

				case "model.finish":
					stopReason := "end_turn"
					if finishReason, ok := msg.Event["finishReason"].(string); ok {
						switch finishReason {
						case "tool-calls":
							stopReason = "tool_use"
						case "stop", "end_turn":
							stopReason = "end_turn"
						}
					}
					finishResponse(stopReason)
				}
			}, logger)

			if err != nil {
				log.Printf("[%s] Error: %v", requestID, err)
				if currentAccount != nil && h.loadBalancer != nil {
					// 记录失败
					h.loadBalancer.ScheduleFailureCount(currentAccount.ID)
					failedAccountIDs = append(failedAccountIDs, currentAccount.ID)
					retryCount++

					// 检查是否超过最大重试次数
					if retryCount >= MaxRetryCount {
						log.Printf("[%s] 已达到最大重试次数 (%d)，停止重试", requestID, MaxRetryCount)
						finishResponse("end_turn")
						break
					}

					log.Printf("[%s] 账号 %s 请求失败，尝试切换账号 (重试 %d/%d, 已排除 %d 个)",
						requestID, currentAccount.Name, retryCount, MaxRetryCount, len(failedAccountIDs))

					// 指数退避：100ms, 200ms, 400ms...
					backoff := time.Duration(1<<(retryCount-1)) * BaseRetryDelay
					log.Printf("[%s] 等待 %v 后重试...", requestID, backoff)
					time.Sleep(backoff)

					if retryErr := selectAccount(); retryErr == nil {
						log.Printf("[%s] 切换到账号: %s，重新发送请求", requestID, currentAccount.Name)
						continue
					} else {
						log.Printf("[%s] 无更多可用账号: %v", requestID, retryErr)
					}
				}
				finishResponse("end_turn")
			} else {
				// 请求成功，记录成功计数并标记账号为活跃
				if currentAccount != nil && h.loadBalancer != nil {
					h.loadBalancer.ScheduleSuccessCount(currentAccount.ID)
				}
				if currentAccount != nil && h.keeper != nil {
					h.keeper.MarkAccountActive(currentAccount.ID)
				}
			}
			break
		}
	}()

	<-done

	if !hasReturn {
		finishResponse("end_turn")
	}

	if !isStream {
		stopReason := finalStopReason
		if stopReason == "" {
			stopReason = "end_turn"
		}

		if len(contentBlocks) == 0 && responseText.Len() > 0 {
			contentBlocks = append(contentBlocks, map[string]interface{}{
				"type": "text",
				"text": responseText.String(),
			})
		}

		usageResp := map[string]int{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		}
		if hasCacheControl {
			usageResp["cache_creation_input_tokens"] = cacheCreationTokens
			usageResp["cache_read_input_tokens"] = cacheReadTokens
		}

		response := map[string]interface{}{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       contentBlocks,
			"model":         req.Model,
			"stop_reason":   stopReason,
			"stop_sequence": nil,
			"usage":         usageResp,
		}
		_ = json.NewEncoder(w).Encode(response)
	}
	_ = finalStopReason
}
