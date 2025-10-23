package mcp

import (
	"encoding/json"
	"fmt"
	"strconv"
)

type jsonrpcMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *jsonrpcError    `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func marshalRequest(id interface{}, method string, params interface{}) ([]byte, error) {
	payload := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		payload["params"] = params
	}
	return json.Marshal(payload)
}

func marshalNotification(method string, params interface{}) ([]byte, error) {
	payload := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		payload["params"] = params
	}
	return json.Marshal(payload)
}

func idToString(raw *json.RawMessage) (string, error) {
	if raw == nil {
		return "", fmt.Errorf("nil id")
	}
	var (
		str string
		num float64
	)
	if err := json.Unmarshal(*raw, &str); err == nil {
		return str, nil
	}
	if err := json.Unmarshal(*raw, &num); err == nil {
		return strconv.FormatInt(int64(num), 10), nil
	}
	return "", fmt.Errorf("unsupported id type: %s", string(*raw))
}
