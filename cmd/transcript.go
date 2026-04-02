package cmd

import (
	"encoding/json"
	"io"
	"os"
)

// tailBytes is the maximum number of bytes to read from the end of a transcript
// file when looking for the last user message. 256KB is enough for several
// conversation turns while keeping memory usage bounded even under heavy
// concurrent hook invocations.
const tailBytes = 256 * 1024

// lastUserMsgHash reads only the tail of the transcript file and returns a
// short hash (first 200 chars of the last user message text). This avoids
// loading multi-hundred-MB transcript files fully into memory — the previous
// implementation caused 200GB+ RAM usage when many hook processes ran in
// parallel.
func lastUserMsgHash(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	// Determine file size
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	size := fi.Size()

	// Read only the last tailBytes of the file
	offset := int64(0)
	readLen := size
	if size > tailBytes {
		offset = size - tailBytes
		readLen = tailBytes
	}

	buf := make([]byte, readLen)
	if _, err := f.ReadAt(buf, offset); err != nil && err != io.EOF {
		return ""
	}

	// Split into lines and scan backwards for the last user message
	lines := splitLines(buf)
	for i := len(lines) - 1; i >= 0; i-- {
		var entry map[string]any
		if json.Unmarshal(lines[i], &entry) != nil {
			continue
		}
		msg, ok := entry["message"].(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "user" {
			continue
		}
		return extractUserHash(msg)
	}
	return ""
}

// extractUserHash returns the first 200 chars of a user message's text content.
func extractUserHash(msg map[string]any) string {
	switch content := msg["content"].(type) {
	case string:
		return content[:min(len(content), 200)]
	case []any:
		for _, item := range content {
			if m, ok := item.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					return t[:min(len(t), 200)]
				}
			}
		}
	}
	return ""
}

// splitLines splits a byte slice into individual lines without converting
// the entire thing to a string (avoids doubling memory).
func splitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			if i > start {
				lines = append(lines, data[start:i])
			}
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}
