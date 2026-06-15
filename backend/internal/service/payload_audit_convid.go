package service

import (
	"strings"

	"github.com/tidwall/gjson"
)

// ExtractRequestConvIDs pulls conversation signals from the upstream REQUEST body.
// Only the OpenAI /v1/responses family carries a stable session key today.
func ExtractRequestConvIDs(endpoint string, reqBody []byte) (convKey, prevRespID string) {
	if !strings.Contains(endpoint, "/responses") || len(reqBody) == 0 {
		return "", ""
	}
	convKey = gjson.GetBytes(reqBody, "prompt_cache_key").String()
	prevRespID = gjson.GetBytes(reqBody, "previous_response_id").String()
	return convKey, prevRespID
}

// ExtractResponseID pulls the upstream response id from the response.
// For /v1/responses it scans the SSE for response.id; for chat it reads top-level id.
func ExtractResponseID(endpoint string, out []byte) string {
	if len(out) == 0 {
		return ""
	}
	if strings.Contains(endpoint, "/responses") {
		// scan each SSE data: line for a response.id
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			data, ok := strings.CutPrefix(line, "data:")
			if !ok {
				continue
			}
			data = strings.TrimSpace(data)
			if data == "" || data == "[DONE]" {
				continue
			}
			if id := gjson.Get(data, "response.id").String(); id != "" {
				return id
			}
			if id := gjson.Get(data, "id").String(); id != "" && strings.HasPrefix(id, "resp_") {
				return id
			}
		}
		return ""
	}
	return gjson.GetBytes(out, "id").String()
}
