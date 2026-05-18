// a_client.go — consumer. Discovers a provider by skill tag, calls them
// with A2A message/send, and polls tasks/get until the reply arrives.
//
// Run: BROKER_URL=http://www.cybertron.studio/a2a go run a_client.go "hello"
//
// Uses only the Go standard library — no ADK dependency required. If you
// already use the ADK client library, simply point it at
//     baseURL = BROKER_URL + "/agents/<provider_id>"
// and send message/send as you would to any native A2A agent.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	broker := env("BROKER_URL", "http://www.cybertron.studio/a2a")
	tag := env("SKILL", "echo")
	text := "hello from A"
	if len(os.Args) > 1 {
		text = os.Args[1]
	}

	providerID := discover(broker, tag)
	fmt.Printf("🔎 found provider %s\n", providerID)

	taskID := send(broker, providerID, text)
	fmt.Printf("📤 task %s submitted\n", taskID)

	reply := pollUntilDone(broker, providerID, taskID, 30*time.Second)
	fmt.Printf("💬 reply: %s\n", reply)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func discover(broker, tag string) string {
	resp, err := http.Get(broker + "/registry/agents?skill=" + tag + "&available=now")
	if err != nil {
		log.Fatalf("discover: %v", err)
	}
	defer resp.Body.Close()
	var list []struct{ AgentID string `json:"agent_id"` }
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		log.Fatalf("decode list: %v", err)
	}
	if len(list) == 0 {
		log.Fatalf("no online provider with skill=%s", tag)
	}
	return list[0].AgentID
}

func send(broker, providerID, text string) string {
	req := map[string]any{
		"jsonrpc": "2.0", "id": "1", "method": "message/send",
		"params": map[string]any{
			"message": map[string]any{
				"messageId": fmt.Sprintf("m-%d", time.Now().UnixMilli()),
				"role":      "ROLE_USER",
				"parts":     []map[string]any{{"text": text}},
			},
		},
	}
	return rpcResult(broker, providerID, req)["id"].(string)
}

func pollUntilDone(broker, providerID, taskID string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		req := map[string]any{
			"jsonrpc": "2.0", "id": "2", "method": "tasks/get",
			"params": map[string]any{"id": taskID},
		}
		r := rpcResult(broker, providerID, req)
		status := r["status"].(map[string]any)
		state := status["state"].(string)
		fmt.Printf("   state=%s\n", state)
		if state == "TASK_STATE_COMPLETED" || state == "TASK_STATE_FAILED" {
			if msg, ok := status["message"].(map[string]any); ok {
				for _, p := range msg["parts"].([]any) {
					if t, ok := p.(map[string]any)["text"].(string); ok {
						return t
					}
				}
			}
			return "(no text in reply)"
		}
	}
	log.Fatalf("timed out")
	return ""
}

func rpcResult(broker, providerID string, req map[string]any) map[string]any {
	raw, _ := json.Marshal(req)
	resp, err := http.Post(broker+"/agents/"+providerID+"/a2a", "application/json", bytes.NewReader(raw))
	if err != nil {
		log.Fatalf("rpc: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var env struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		log.Fatalf("decode: %v body=%s", err, body)
	}
	if env.Error != nil {
		log.Fatalf("jsonrpc error: %d %s", env.Error.Code, env.Error.Message)
	}
	return env.Result
}
