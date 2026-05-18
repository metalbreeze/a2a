// b_agent.go — provider agent. Registers with the broker, holds an SSE
// connection for incoming tasks, and echoes each task back.
//
// Run: BROKER_URL=http://www.cybertron.studio/a2a go run b_agent.go
//
// This file has no third-party dependencies; it speaks raw HTTP and parses
// JSON with the standard library, so you can drop it into any Go project.
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
}

type inboxMsg struct {
	TaskID    string          `json:"task_id"`
	ContextID string          `json:"context_id"`
	Payload   json.RawMessage `json:"payload"`
}

func main() {
	broker := env("BROKER_URL", "http://www.cybertron.studio/a2a")

	reg := register(broker, "echo-b", "Echo provider", "echo")
	fmt.Printf("registered agent_id=%s\ntoken=%s\n\n", reg.AgentID, reg.Token)

	listen(broker, reg)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func register(broker, name, description, skillTag string) registerResp {
	body := map[string]any{
		"name":        name,
		"description": description,
		"version":     "0.1.0",
		"mode":        "realtime",
		"card": map[string]any{
			"name":            name,
			"description":     description,
			"version":         "0.1.0",
			"protocolVersion": "0.3.0",
			"capabilities":    map[string]bool{"streaming": false},
			"defaultInputModes":  []string{"text/plain"},
			"defaultOutputModes": []string{"text/plain"},
			"skills": []map[string]any{
				{"id": skillTag, "name": skillTag, "tags": []string{skillTag}},
			},
		},
	}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(broker+"/registry/agents", "application/json", bytes.NewReader(raw))
	if err != nil {
		log.Fatalf("register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		log.Fatalf("register status=%d body=%s", resp.StatusCode, b)
	}
	var r registerResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		log.Fatalf("decode: %v", err)
	}
	return r
}

func listen(broker string, reg registerResp) {
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/agents/%s/inbox/stream", broker, reg.AgentID), nil)
	req.Header.Set("Authorization", "Bearer "+reg.Token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		log.Fatalf("stream status=%d body=%s", resp.StatusCode, b)
	}
	fmt.Println("📡 listening (Ctrl+C to quit)")

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var event, data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "":
			if event == "task" && data != "" {
				var m inboxMsg
				if err := json.Unmarshal([]byte(data), &m); err == nil {
					go handle(broker, reg, &m)
				}
			}
			event, data = "", ""
		}
	}
}

func handle(broker string, reg registerResp, m *inboxMsg) {
	// Parse the user's text out of the payload.
	var p struct {
		Message struct {
			Parts []struct {
				Text *string `json:"text,omitempty"`
			} `json:"parts"`
		} `json:"message"`
	}
	_ = json.Unmarshal(m.Payload, &p)
	user := ""
	for _, pt := range p.Message.Parts {
		if pt.Text != nil {
			user += *pt.Text
		}
	}
	fmt.Printf("📥 task %s  user=%q\n", m.TaskID, user)

	time.Sleep(100 * time.Millisecond) // pretend to do work
	reply := "Echo: " + user

	body, _ := json.Marshal(map[string]any{
		"state": "TASK_STATE_COMPLETED",
		"message": map[string]any{
			"messageId": m.TaskID + "-r",
			"role":      "ROLE_AGENT",
			"parts":     []map[string]any{{"text": reply}},
		},
	})
	req, _ := http.NewRequest("POST",
		fmt.Sprintf("%s/agents/%s/tasks/%s/result", broker, reg.AgentID, m.TaskID),
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+reg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("reply: %v", err)
		return
	}
	resp.Body.Close()
	fmt.Printf("   ✅ posted reply\n")
}
