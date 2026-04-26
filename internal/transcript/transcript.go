package transcript

import "strings"

type Message struct {
	Role string
	Text string
}

func ParseLine(entry map[string]any) (Message, bool) {
	if msg, ok := entry["message"].(map[string]any); ok {
		role, _ := msg["role"].(string)
		if role == "" {
			role, _ = entry["type"].(string)
		}
		text := ExtractText(msg)
		if role != "" && text != "" {
			return Message{Role: role, Text: text}, true
		}
	}

	payload, _ := entry["payload"].(map[string]any)
	if payload == nil {
		return Message{}, false
	}

	switch typ, _ := payload["type"].(string); typ {
	case "message":
		role, _ := payload["role"].(string)
		text := ExtractText(payload)
		if role != "" && text != "" {
			return Message{Role: role, Text: text}, true
		}
	case "user_message":
		if text, _ := payload["message"].(string); text != "" {
			return Message{Role: "user", Text: text}, true
		}
	case "agent_message":
		if text, _ := payload["message"].(string); text != "" {
			return Message{Role: "assistant", Text: text}, true
		}
	}

	return Message{}, false
}

func ExtractText(msg map[string]any) string {
	switch content := msg["content"].(type) {
	case string:
		return content
	case []any:
		var texts []string
		for _, item := range content {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := m["text"].(string); ok && t != "" {
				texts = append(texts, t)
				continue
			}
			if t, ok := m["input_text"].(string); ok && t != "" {
				texts = append(texts, t)
				continue
			}
			if t, ok := m["output_text"].(string); ok && t != "" {
				texts = append(texts, t)
				continue
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}
