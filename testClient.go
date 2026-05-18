//go:build ignore

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	client "github.com/inference-gateway/adk/client"
	types "github.com/inference-gateway/adk/types"
	zap "go.uber.org/zap"
)

const serverURL = "http://www.cybertron.studio:6321"

func main() {
	ctx := context.Background()

	logger, err := zap.NewDevelopment()
	if err != nil {
		log.Fatalf("failed to create logger: %v", err)
	}
	defer logger.Sync()

	a2aClient := client.NewClientWithConfig(&client.Config{
		BaseURL:    serverURL,
		Timeout:    30 * time.Second,
		UserAgent:  "TestClient/1.0",
		MaxRetries: 2,
		RetryDelay: time.Second,
		Logger:     logger,
	})

	// 1. 获取 Agent Card
	fmt.Println("\n=== Agent Card ===")
	agentCard, err := a2aClient.GetAgentCard(ctx)
	if err != nil {
		logger.Fatal("failed to get agent card", zap.Error(err))
	}
	cardJSON, _ := json.MarshalIndent(agentCard, "", "  ")
	fmt.Println(string(cardJSON))

	// 2. 发送测试消息
	fmt.Println("\n=== Send Task ===")
	msgID := fmt.Sprintf("msg-%d", time.Now().UnixMilli())
	params := types.MessageSendParams{
		Message: types.Message{
			MessageID: msgID,
			Role:      types.RoleUser,
			Parts: []types.Part{
				types.CreateTextPart("Hello from test client! Please echo this message back."),
			},
		},
		Configuration: &types.MessageSendConfiguration{
			Blocking: func() *bool { b := true; return &b }(),
		},
	}

	resp, err := a2aClient.SendTask(ctx, params)
	if err != nil {
		logger.Fatal("failed to send task", zap.Error(err))
	}

	helper := a2aClient.GetArtifactHelper()
	task, err := helper.ExtractTaskFromResponse(resp)
	if err != nil {
		logger.Fatal("failed to extract task", zap.Error(err))
	}

	fmt.Printf("Task ID:    %s\n", task.ID)
	fmt.Printf("Context ID: %s\n", task.ContextID)
	fmt.Printf("State:      %s\n", task.Status.State)

	if task.Status.Message != nil {
		for _, part := range task.Status.Message.Parts {
			if part.Text != nil {
				fmt.Printf("Reply:      %s\n", *part.Text)
			}
		}
	}

	for i, artifact := range task.Artifacts {
		fmt.Printf("Artifact[%d]: %s\n", i, artifact.ArtifactID)
		for _, part := range artifact.Parts {
			if part.Text != nil {
				fmt.Printf("  Text: %s\n", *part.Text)
			}
		}
	}

	// 3. 流式测试
	fmt.Println("\n=== Streaming Task ===")
	streamMsgID := fmt.Sprintf("msg-stream-%d", time.Now().UnixMilli())
	streamParams := types.MessageSendParams{
		Message: types.Message{
			MessageID: streamMsgID,
			Role:      types.RoleUser,
			Parts: []types.Part{
				types.CreateTextPart("Streaming test: count from 1 to 3."),
			},
		},
	}

	eventChan, err := a2aClient.SendTaskStreaming(ctx, streamParams)
	if err != nil {
		logger.Fatal("failed to start streaming", zap.Error(err))
	}

	for event := range eventChan {
		t, err := helper.ExtractTaskFromResponse(&event)
		if err != nil {
			continue
		}
		fmt.Printf("[stream] state=%s\n", t.Status.State)
		if t.Status.Message != nil {
			for _, part := range t.Status.Message.Parts {
				if part.Text != nil {
					fmt.Printf("  text: %s\n", *part.Text)
				}
			}
		}
	}

	fmt.Println("\n=== Done ===")
}
