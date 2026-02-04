package prompt

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url,omitempty"`
}

type CacheControl struct {
	Type string `json:"type"`
}

type ContentBlock struct {
	Type   string       `json:"type"`
	Text   string       `json:"text,omitempty"`
	Source *ImageSource `json:"source,omitempty"`

	ID    string      `json:"id,omitempty"`
	Name  string      `json:"name,omitempty"`
	Input interface{} `json:"input,omitempty"`

	ToolUseID    string        `json:"tool_use_id,omitempty"`
	Content      interface{}   `json:"content,omitempty"`
	IsError      bool          `json:"is_error,omitempty"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`

	Thinking string `json:"thinking,omitempty"`
}

type MessageContent struct {
	Text   string
	Blocks []ContentBlock
}

func (mc *MessageContent) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		mc.Text = text
		mc.Blocks = nil
		return nil
	}

	var blocks []ContentBlock
	if err := json.Unmarshal(data, &blocks); err == nil {
		mc.Text = ""
		mc.Blocks = blocks
		return nil
	}

	return fmt.Errorf("content must be string or array of content blocks")
}

func (mc MessageContent) MarshalJSON() ([]byte, error) {
	if mc.Blocks != nil {
		return json.Marshal(mc.Blocks)
	}
	return json.Marshal(mc.Text)
}

func (mc *MessageContent) IsString() bool {
	return mc.Blocks == nil
}

func (mc *MessageContent) GetText() string {
	return mc.Text
}

func (mc *MessageContent) GetBlocks() []ContentBlock {
	return mc.Blocks
}

type Message struct {
	Role    string         `json:"role"`
	Content MessageContent `json:"content"`
}

type SystemItem struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

type ToolInputSchema struct {
	Type       string                 `json:"type"`
	Properties map[string]interface{} `json:"properties,omitempty"`
	Required   []string               `json:"required,omitempty"`
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema ToolInputSchema `json:"input_schema"`
}

type ClaudeAPIRequest struct {
	Model     string        `json:"model"`
	Messages  []Message     `json:"messages"`
	System    []SystemItem  `json:"system"`
	Tools     []interface{} `json:"tools"`
	Stream    bool          `json:"stream"`
	MaxTokens int           `json:"max_tokens,omitempty"`
	Thinking  *struct {
		Type        string `json:"type"`
		BudgetToken int    `json:"budget_tokens"`
	} `json:"thinking,omitempty"`
}

type ImageData struct {
	Format string `json:"format"`
	Data   string `json:"data"`
}

func ExtractImages(content MessageContent) []ImageData {
	var images []ImageData
	if content.IsString() {
		return images
	}
	for _, block := range content.GetBlocks() {
		if block.Type == "image" && block.Source != nil {
			if block.Source.Type == "base64" && block.Source.Data != "" {
				format := "png"
				if block.Source.MediaType != "" {
					parts := strings.Split(block.Source.MediaType, "/")
					if len(parts) == 2 {
						format = parts[1]
					}
				}
				images = append(images, ImageData{
					Format: format,
					Data:   block.Source.Data,
				})
			}
		}
	}
	return images
}

func ExtractToolResults(content MessageContent) []map[string]interface{} {
	var results []map[string]interface{}
	if content.IsString() {
		return results
	}
	for _, block := range content.GetBlocks() {
		if block.Type == "tool_result" {
			result := map[string]interface{}{
				"tool_use_id": block.ToolUseID,
				"content":     serializeContent(block.Content),
				"is_error":    block.IsError,
			}
			results = append(results, result)
		}
	}
	return results
}

func ExtractToolUses(content MessageContent) []map[string]interface{} {
	var toolUses []map[string]interface{}
	if content.IsString() {
		return toolUses
	}
	for _, block := range content.GetBlocks() {
		if block.Type == "tool_use" {
			toolUses = append(toolUses, map[string]interface{}{
				"id":    block.ID,
				"name":  block.Name,
				"input": block.Input,
			})
		}
	}
	return toolUses
}

func serializeContent(content interface{}) string {
	if content == nil {
		return ""
	}
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		var parts []string
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				if text, ok := m["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
		jsonBytes, _ := json.Marshal(v)
		return string(jsonBytes)
	default:
		jsonBytes, _ := json.Marshal(v)
		return string(jsonBytes)
	}
}

func HasCacheControl(system []SystemItem) bool {
	for _, s := range system {
		if s.CacheControl != nil && s.CacheControl.Type == "ephemeral" {
			return true
		}
	}
	return false
}

func ImageToBase64Tag(img ImageData) string {
	return fmt.Sprintf("[IMAGE: %s, size=%d bytes]", img.Format, len(img.Data)*3/4)
}

func DecodeBase64Image(data string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(data)
}

const systemPreset = `你是 AI 编程助手。

## 对话历史结构
- <turn index="N" role="user|assistant"> 包含每轮对话
- <tool_use id="..." name="..."> 表示工具调用
- <tool_result tool_use_id="..."> 表示工具执行结果
- [IMAGE: format] 表示用户发送的图片

## 规则
1. 仅依赖当前工具和历史上下文
2. 用户在本地环境工作
3. 回复简洁专业
4. 工具调用时使用正确的 JSON 格式`

func FormatMessagesAsMarkdown(messages []Message) string {
	if len(messages) == 0 {
		return ""
	}

	var parts []string
	historyMessages := messages
	if len(messages) > 0 && messages[len(messages)-1].Role == "user" {
		historyMessages = messages[:len(messages)-1]
	}

	turnIndex := 1
	for _, msg := range historyMessages {
		switch msg.Role {
		case "user":
			userContent := formatUserMessage(msg.Content)
			if userContent != "" {
				parts = append(parts, fmt.Sprintf("<turn index=\"%d\" role=\"user\">\n%s\n</turn>", turnIndex, userContent))
				turnIndex++
			}
		case "assistant":
			assistantContent := formatAssistantMessage(msg.Content)
			if assistantContent != "" {
				parts = append(parts, fmt.Sprintf("<turn index=\"%d\" role=\"assistant\">\n%s\n</turn>", turnIndex, assistantContent))
				turnIndex++
			}
		}
	}

	if len(parts) == 0 {
		return ""
	}

	return strings.Join(parts, "\n\n")
}

func formatUserMessage(content MessageContent) string {
	var parts []string

	if content.IsString() {
		text := strings.TrimSpace(content.GetText())
		if text != "" {
			parts = append(parts, text)
		}
		return strings.Join(parts, "\n")
	}

	for _, block := range content.GetBlocks() {
		switch block.Type {
		case "text":
			text := strings.TrimSpace(block.Text)
			if text != "" {
				parts = append(parts, text)
			}
		case "image":
			if block.Source != nil {
				format := "png"
				if block.Source.MediaType != "" {
					ps := strings.Split(block.Source.MediaType, "/")
					if len(ps) == 2 {
						format = ps[1]
					}
				}
				parts = append(parts, fmt.Sprintf("[IMAGE: %s]", format))
			}
		case "tool_result":
			resultStr := serializeContent(block.Content)
			errorAttr := ""
			if block.IsError {
				errorAttr = ` is_error="true"`
			}
			parts = append(parts, fmt.Sprintf("<tool_result tool_use_id=\"%s\"%s>\n%s\n</tool_result>", block.ToolUseID, errorAttr, resultStr))
		}
	}

	return strings.Join(parts, "\n")
}

func formatAssistantMessage(content MessageContent) string {
	var parts []string

	if content.IsString() {
		text := strings.TrimSpace(content.GetText())
		if text != "" {
			parts = append(parts, text)
		}
		return strings.Join(parts, "\n")
	}

	for _, block := range content.GetBlocks() {
		switch block.Type {
		case "text":
			text := strings.TrimSpace(block.Text)
			if text != "" {
				parts = append(parts, text)
			}
		case "thinking":
			continue
		case "tool_use":
			inputJSON, _ := json.Marshal(block.Input)
			parts = append(parts, fmt.Sprintf("<tool_use id=\"%s\" name=\"%s\">\n%s\n</tool_use>", block.ID, block.Name, string(inputJSON)))
		}
	}

	return strings.Join(parts, "\n")
}

func FormatToolsForPrompt(tools []interface{}) string {
	if len(tools) == 0 {
		return ""
	}
	var toolDescs []string
	for _, t := range tools {
		if tm, ok := t.(map[string]interface{}); ok {
			name, _ := tm["name"].(string)
			desc, _ := tm["description"].(string)
			if name != "" {
				if desc != "" {
					toolDescs = append(toolDescs, fmt.Sprintf("- %s: %s", name, desc))
				} else {
					toolDescs = append(toolDescs, fmt.Sprintf("- %s", name))
				}
			}
		}
	}
	return strings.Join(toolDescs, "\n")
}

func BuildPromptV2(req ClaudeAPIRequest) string {
	var sections []string

	var clientSystem []string
	for _, s := range req.System {
		if s.Type == "text" && s.Text != "" {
			clientSystem = append(clientSystem, s.Text)
		}
	}
	if len(clientSystem) > 0 {
		sections = append(sections, fmt.Sprintf("<client_system>\n%s\n</client_system>", strings.Join(clientSystem, "\n\n")))
	}

	sections = append(sections, fmt.Sprintf("<proxy_instructions>\n%s\n</proxy_instructions>", systemPreset))

	if len(req.Tools) > 0 {
		toolsDesc := FormatToolsForPrompt(req.Tools)
		if toolsDesc != "" {
			sections = append(sections, fmt.Sprintf("<available_tools>\n%s\n</available_tools>", toolsDesc))
		}
	}

	history := FormatMessagesAsMarkdown(req.Messages)
	if history != "" {
		sections = append(sections, fmt.Sprintf("<conversation_history>\n%s\n</conversation_history>", history))
	}

	var currentRequest string
	if len(req.Messages) > 0 {
		lastMsg := req.Messages[len(req.Messages)-1]
		if lastMsg.Role == "user" {
			currentRequest = formatUserMessage(lastMsg.Content)
			images := ExtractImages(lastMsg.Content)
			if len(images) > 0 {
				var imgTags []string
				for _, img := range images {
					imgTags = append(imgTags, ImageToBase64Tag(img))
				}
				currentRequest += "\n" + strings.Join(imgTags, "\n")
			}
		}
	}
	if strings.TrimSpace(currentRequest) == "" {
		currentRequest = "继续"
	}

	sections = append(sections, fmt.Sprintf("<user_request>\n%s\n</user_request>", currentRequest))

	return strings.Join(sections, "\n\n")
}

func SummarizeHistory(messages []Message, maxTokens int) []Message {
	if len(messages) <= 10 {
		return messages
	}
	kept := messages[len(messages)-10:]
	older := messages[:len(messages)-10]

	var summaryParts []string
	for _, msg := range older {
		var text string
		if msg.Content.IsString() {
			text = msg.Content.GetText()
		} else {
			for _, b := range msg.Content.GetBlocks() {
				if b.Type == "text" {
					text += b.Text + " "
				}
			}
		}
		if len(text) > 200 {
			text = text[:200] + "..."
		}
		if text != "" {
			summaryParts = append(summaryParts, fmt.Sprintf("[%s]: %s", msg.Role, strings.TrimSpace(text)))
		}
	}

	if len(summaryParts) > 0 {
		summaryText := "Earlier conversation summary:\n" + strings.Join(summaryParts, "\n")
		summaryMsg := Message{
			Role:    "user",
			Content: MessageContent{Text: summaryText},
		}
		return append([]Message{summaryMsg}, kept...)
	}

	return kept
}
