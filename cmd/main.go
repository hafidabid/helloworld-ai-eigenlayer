package main

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

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

	// Validate task ID is not empty
	if len(t.TaskId) == 0 {
		return fmt.Errorf("task ID cannot be empty")
	}

	// Validate payload is not empty
	if len(t.Payload) == 0 {
		return fmt.Errorf("task payload cannot be empty")
	}

	// Validate payload size (prevent extremely large prompts)
	maxPayloadSize := 4096 // 4KB limit
	if len(t.Payload) > maxPayloadSize {
		return fmt.Errorf("task payload size %d exceeds maximum allowed size %d", len(t.Payload), maxPayloadSize)
	}

	if !bytes.Contains(t.Payload, []byte{0}) {

		if !utf8.Valid(t.Payload) {
			return fmt.Errorf("task payload contains invalid UTF-8 characters")
		}
	}

	prompt := string(t.Payload)
	if len(strings.TrimSpace(prompt)) == 0 {
		return fmt.Errorf("task prompt cannot be empty or whitespace only")
	}

	maliciousPatterns := []string{
		"<script>", "</script>", "javascript:", "data:text/html",
		"eval(", "exec(", "system(", "rm -rf", "DROP TABLE",
	}

	for _, pattern := range maliciousPatterns {
		if strings.Contains(strings.ToLower(prompt), strings.ToLower(pattern)) {
			return fmt.Errorf("task payload contains potentially malicious content: %s", pattern)
		}
	}

	// Validate Azure OpenAI environment variables are set
	apiKey := os.Getenv("AZURE_OPENAI_KEY")
	endpoint := os.Getenv("AZURE_OPENAI_ENDPOINT")
	if apiKey == "" || endpoint == "" {
		return fmt.Errorf("Azure OpenAI configuration not properly set")
	}

	// Validate endpoint format
	if !strings.HasPrefix(endpoint, "https://") {
		return fmt.Errorf("Azure OpenAI endpoint must use HTTPS")
	}

	tw.logger.Sugar().Infow("Task validation passed",
		zap.String("taskId", string(t.TaskId)),
		zap.Int("payloadSize", len(t.Payload)),
	)

	return nil
}

func (tw *TaskWorker) ValidateResult(resultBytes []byte) error {
	// Validate result is not empty
	if len(resultBytes) == 0 {
		return fmt.Errorf("result cannot be empty")
	}

	// Validate result size (prevent extremely large results)
	maxResultSize := 8192 // 8KB limit
	if len(resultBytes) > maxResultSize {
		return fmt.Errorf("result size %d exceeds maximum allowed size %d", len(resultBytes), maxResultSize)
	}

	// Validate result is valid JSON
	var result map[string]interface{}
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		return fmt.Errorf("result is not valid JSON: %w", err)
	}

	// Validate required fields exist
	requiredFields := []string{"llm_output", "verified"}
	for _, field := range requiredFields {
		if _, exists := result[field]; !exists {
			return fmt.Errorf("result missing required field: %s", field)
		}
	}

	// Validate llm_output is a string
	if llmOutput, ok := result["llm_output"].(string); !ok {
		return fmt.Errorf("llm_output field must be a string")
	} else {
		// Validate llm_output is not empty
		if len(strings.TrimSpace(llmOutput)) == 0 {
			return fmt.Errorf("llm_output cannot be empty or whitespace only")
		}
	}

	// Validate verified is a boolean
	if _, ok := result["verified"].(bool); !ok {
		return fmt.Errorf("verified field must be a boolean")
	}

	tw.logger.Sugar().Infow("Result validation passed",
		zap.Int("resultSize", len(resultBytes)),
	)

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

	// Validate the result before returning
	if err := tw.ValidateResult(resultBytes); err != nil {
		return nil, fmt.Errorf("result validation failed: %w", err)
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
