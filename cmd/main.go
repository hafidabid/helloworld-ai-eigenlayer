package main

import (
	"context"
	"fmt"
	"time"

	"bytes"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"os"

	"github.com/Layr-Labs/hourglass-monorepo/ponos/pkg/performer/server"
	performerV1 "github.com/Layr-Labs/protocol-apis/gen/protos/eigenlayer/hourglass/v1/performer"
	"go.uber.org/zap"
)

// This offchain binary is run by Operators running the Hourglass Executor. It contains
// the business logic of the AVS and performs worked based on the tasked sent to it.
// The Hourglass Aggregator ingests tasks from the TaskMailbox and distributes work
// to Executors configured to run the AVS Performer. Performers execute the work and
// return the result to the Executor where the result is signed and return to the
// Aggregator to place in the outbox once the signing threshold is met.

type TaskWorker struct {
	logger *zap.Logger
}

func NewTaskWorker(logger *zap.Logger) *TaskWorker {
	return &TaskWorker{
		logger: logger,
	}
}

func (tw *TaskWorker) ValidateTask(t *performerV1.TaskRequest) error {
	tw.logger.Sugar().Infow("Validating task",
		zap.Any("task", t),
	)

	// ------------------------------------------------------------------------
	// Implement your AVS task validation logic here
	// ------------------------------------------------------------------------
	// This is where the Perfomer will validate the task request data.
	// E.g. the Perfomer may validate that the request params are well formed and adhere to a schema.

	return nil
}

func (tw *TaskWorker) HandleTask(t *performerV1.TaskRequest) (*performerV1.TaskResponse, error) {
	tw.logger.Sugar().Infow("Handling task",
		zap.Any("task", t),
	)

	// Call Azure OpenAI LLM
	apiKey := os.Getenv("AZURE_OPENAI_KEY")
	endpoint := os.Getenv("AZURE_OPENAI_ENDPOINT")
	if apiKey == "" || endpoint == "" {
		return nil, fmt.Errorf("Azure OpenAI API key or endpoint not set")
	}

	prompt := string(t.Payload)
	requestBody, err := json.Marshal(map[string]interface{}{
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens":  64,
		"temperature": 0.2,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", endpoint, bytes.NewBuffer(requestBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", apiKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var llmResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &llmResp); err != nil {
		return nil, err
	}

	llmOutput := ""
	if len(llmResp.Choices) > 0 {
		llmOutput = llmResp.Choices[0].Message.Content
	}

	// Simple AI-based verification: check if output contains 'valid'
	verified := false
	if llmOutput != "" && bytes.Contains([]byte(llmOutput), []byte("valid")) {
		verified = true
	}

	result := map[string]interface{}{
		"llm_output": llmOutput,
		"verified":   verified,
	}
	resultBytes, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}

	return &performerV1.TaskResponse{
		TaskId: t.TaskId,
		Result: resultBytes,
	}, nil
}

func main() {
	ctx := context.Background()
	l, _ := zap.NewProduction()

	w := NewTaskWorker(l)

	pp, err := server.NewPonosPerformerWithRpcServer(&server.PonosPerformerConfig{
		Port:    8080,
		Timeout: 5 * time.Second,
	}, w, l)
	if err != nil {
		panic(fmt.Errorf("failed to create performer: %w", err))
	}

	if err := pp.Start(ctx); err != nil {
		panic(err)
	}
}
