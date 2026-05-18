// b_agent — simulates a client-B agent that has no public IP.
// It registers with the broker, opens an SSE stream to receive tasks,
// and echoes them back.
//
//   go run ./cmd/b_agent
//
// Env:
//   BROKER_URL (default http://localhost:6321)
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type registerResp struct {
	AgentID string `json:"agent_id"`
	Token   string `json:"token"`
	A2AURL  string `json:"a2a_url"`
	CardURL string `json:"card_url"`
}

type inboxMsg struct {
	TaskID    string          `json:"task_id"`
	ContextID string          `json:"context_id"`
	Payload   json.RawMessage `json:"payload"`
}

func main() {
	broker := os.Getenv("BROKER_URL")
	if broker == "" {
		broker = "http://localhost:6321"
	}

	// 1) Register
	body := map[string]any{
		"name":        "echo-b",
		"description": "Echo agent, returns whatever you send.",
		"version":     "0.1.0",
		"mode":        "realtime",
		"card": map[string]any{
			"name":            "echo-b",
			"description":     "Echo agent, returns whatever you send.",
			"version":         "0.1.0",
			"protocolVersion": "0.3.0",
			"capabilities": map[string]bool{
				"streaming":              false,
				"pushNotifications":      false,
				"stateTransitionHistory": false,
			},
			"defaultInputModes":  []string{"text/plain"},
			"defaultOutputModes": []string{"text/plain"},
			"skills": []map[string]any{
				{
					"id":          "echo",
					"name":        "Echo",
					"description": "Returns the same text you send.",
					"tags":        []string{"echo", "test"},
				},
			},
		},
	}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(broker+"/registry/agents", "application/json", bytes.NewReader(raw))
	if err != nil {
		log.Fatalf("register failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		log.Fatalf("register status=%d body=%s", resp.StatusCode, b)
	}
	var reg registerResp
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil {
		log.Fatalf("decode register resp: %v", err)
	}
	fmt.Printf("registered agent_id=%s\n", reg.AgentID)
	fmt.Printf("a2a_url=%s\n", reg.A2AURL)
	fmt.Printf("token=%s\n\n", reg.Token)

	// 2) Open SSE stream
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/agents/%s/inbox/stream", broker, reg.AgentID), nil)
	req.Header.Set("Authorization", "Bearer "+reg.Token)
	req.Header.Set("Accept", "text/event-stream")
	streamResp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatalf("stream connect: %v", err)
	}
	defer streamResp.Body.Close()
	if streamResp.StatusCode != 200 {
		b, _ := io.ReadAll(streamResp.Body)
		log.Fatalf("stream status=%d body=%s", streamResp.StatusCode, b)
	}
	fmt.Println("📡 listening for tasks... (Ctrl+C to quit)")

	scanner := bufio.NewScanner(streamResp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	var eventName, dataLine string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLine = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "":
			if eventName == "task" && dataLine != "" {
				var msg inboxMsg
				if err := json.Unmarshal([]byte(dataLine), &msg); err != nil {
					log.Printf("bad task payload: %v", err)
				} else {
					go handleTask(broker, reg.AgentID, reg.Token, &msg)
				}
			}
			eventName, dataLine = "", ""
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("stream closed: %v", err)
	}
}

type messageSendParams struct {
	Message struct {
		MessageID string          `json:"messageId"`
		Role      string          `json:"role"`
		Parts     json.RawMessage `json:"parts"`
	} `json:"message"`
}

type textPart struct {
	Text *string `json:"text,omitempty"`
}

func handleTask(broker, agentID, token string, msg *inboxMsg) {
	fmt.Printf("\n📥 task %s received\n", msg.TaskID)

	var p messageSendParams
	_ = json.Unmarshal(msg.Payload, &p)
	var parts []textPart
	_ = json.Unmarshal(p.Message.Parts, &parts)
	var userText string
	for _, tp := range parts {
		if tp.Text != nil {
			userText += *tp.Text
		}
	}
	fmt.Printf("   user: %q\n", userText)

	reply := "Echo: " + userText
	fmt.Printf("   replying: %q\n", reply)
	time.Sleep(200 * time.Millisecond) // simulate some work

	resultBody := map[string]any{
		"state": "TASK_STATE_COMPLETED",
		"message": map[string]any{
			"messageId": msg.TaskID + "-reply",
			"role":      "ROLE_AGENT",
			"parts":     []map[string]any{{"text": reply}},
		},
	}
	raw, _ := json.Marshal(resultBody)
	req, _ := http.NewRequest("POST",
		fmt.Sprintf("%s/agents/%s/tasks/%s/result", broker, agentID, msg.TaskID),
		bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("post result: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		log.Printf("post result status=%d body=%s", resp.StatusCode, b)
		return
	}
	fmt.Printf("   ✅ result posted\n")
}
