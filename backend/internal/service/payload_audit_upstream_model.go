package service

import (
	"strings"

	"github.com/tidwall/gjson"
)

// ExtractUpstreamModel pulls the model the upstream ACTUALLY served from the
// response body. This is the ground-truth model after any mapping/rewrite the
// gateway applied, and is stored alongside the client-requested model.
//
// Mirrors ExtractResponseID: for SSE it scans each data: line; for a plain JSON
// body it reads the top-level model. Covers three response shapes:
//   - OpenAI Responses API .............. response.model (inside response.created)
//   - OpenAI chat.completion(.chunk) .... top-level model
//   - Gemini generateContent ............ modelVersion
func ExtractUpstreamModel(endpoint string, out []byte) string {
	if len(out) == 0 {
		return ""
	}
	s := string(out)
	if strings.Contains(s, "data:") {
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimSpace(line)
			data, ok := strings.CutPrefix(line, "data:")
			if !ok {
				continue
			}
			data = strings.TrimSpace(data)
			if data == "" || data == "[DONE]" {
				continue
			}
			if m := modelFromJSON(data); m != "" {
				return m
			}
		}
	}
	return modelFromJSONBytes(out)
}

func modelFromJSON(json string) string {
	if m := gjson.Get(json, "response.model").String(); m != "" {
		return m
	}
	if m := gjson.Get(json, "model").String(); m != "" {
		return m
	}
	return gjson.Get(json, "modelVersion").String()
}

func modelFromJSONBytes(b []byte) string {
	if m := gjson.GetBytes(b, "response.model").String(); m != "" {
		return m
	}
	if m := gjson.GetBytes(b, "model").String(); m != "" {
		return m
	}
	return gjson.GetBytes(b, "modelVersion").String()
}
