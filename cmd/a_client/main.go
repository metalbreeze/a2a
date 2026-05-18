// a_client — simulates a client-A agent discovering B via the broker and
// calling it using the standard ADK A2A client.
//
//   go run ./cmd/a_client "hello from A"
//
// Env:
//   BROKER_URL (default http://localhost:6321)
//   AGENT_ID   (optional; if unset, the first online agent with skill=echo is used)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	client "github.com/inference-gateway/adk/client"
	types "github.com/inference-gateway/adk/types"
	zap "go.uber.org/zap"
)

type discoveredAgent struct {
	AgentID string          `json:"agent_id"`
	Name    string          `json:"name"`
	Card    json.RawMessage `json:"card"`
	Mode    string          `json:"mode"`
	Online  bool            `json:"online"`
	A2AURL  string          `json:"a2a_url"`
}

func main() {
	broker := os.Getenv("BROKER_URL")
	if broker == "" {
		broker = "http://localhost:6321"
	}
	msg := "hello from A"
	if len(os.Args) > 1 {
		msg = os.Args[1]
	}

	// 1) Discover
	agentID := os.Getenv("AGENT_ID")
	var a2aURL string
	if agentID == "" {
		resp, err := http.Get(broker + "/registry/agents?skill=echo&available=now")
		if err != nil {
			log.Fatalf("discover: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var list []discoveredAgent
		if err := json.Unmarshal(b, &list); err != nil {
			log.Fatalf("parse discover: %v body=%s", err, b)
		}
		if len(list) == 0 {
			log.Fatalf("no online agents with skill=echo found (%s)", b)
		}
		agentID = list[0].AgentID
		a2aURL = list[0].A2AURL
		fmt.Printf("🔎 discovered agent: %s (%s)\n", list[0].Name, agentID)
	} else {
		a2aURL = fmt.Sprintf("%s/agents/%s/a2a", broker, agentID)
	}

	// 2) Build an ADK A2A client pointed at the per-agent URL
	// (adk client appends /a2a itself when it calls the JSON-RPC endpoint,
	//  so pass the base path without the trailing /a2a)
	base := a2aURL
	if len(base) > len("/a2a") && base[len(base)-len("/a2a"):] == "/a2a" {
		base = base[:len(base)-len("/a2a")]
	}

	logger, _ := zap.NewDevelopment()
	cli := client.NewClientWithLogger(base, logger)

	// Optional: fetch agent card
	fmt.Println("\n=== Agent Card ===")
	card, err := cli.GetAgentCard(context.Background())
	if err != nil {
		log.Printf("get card failed (non-fatal): %v", err)
	} else {
		cj, _ := json.MarshalIndent(card, "", "  ")
		fmt.Println(string(cj))
	}

	// 3) Send task
	fmt.Println("\n=== Send Task ===")
	params := types.MessageSendParams{
		Message: types.Message{
			MessageID: fmt.Sprintf("a-%d", time.Now().UnixMilli()),
			Role:      types.RoleUser,
			Parts:     []types.Part{types.CreateTextPart(msg)},
		},
	}
	sendResp, err := cli.SendTask(context.Background(), params)
	if err != nil {
		log.Fatalf("send task: %v", err)
	}
	helper := cli.GetArtifactHelper()
	task, err := helper.ExtractTaskFromResponse(sendResp)
	if err != nil {
		log.Fatalf("extract task: %v", err)
	}
	fmt.Printf("Task ID: %s  State: %s\n", task.ID, task.Status.State)

	// 4) Poll tasks/get until completed
	fmt.Println("\n=== Poll until complete ===")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		getResp, err := cli.GetTask(context.Background(), types.TaskQueryParams{ID: task.ID})
		if err != nil {
			log.Printf("get task: %v", err)
			continue
		}
		t, err := helper.ExtractTaskFromResponse(getResp)
		if err != nil {
			log.Printf("extract: %v", err)
			continue
		}
		fmt.Printf("   state=%s\n", t.Status.State)
		if string(t.Status.State) == "TASK_STATE_COMPLETED" || string(t.Status.State) == "TASK_STATE_FAILED" {
			if t.Status.Message != nil {
				for _, p := range t.Status.Message.Parts {
					if p.Text != nil {
						fmt.Printf("💬 reply: %s\n", *p.Text)
					}
				}
			}
			return
		}
	}
	log.Fatalf("timed out waiting for task %s", task.ID)
}
