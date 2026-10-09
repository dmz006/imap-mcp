package enrichment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Embedder turns text into a vector (AGENT.md D11a: always direct Ollama).
type Embedder interface {
	Name() string
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Classifier answers a classification prompt with raw model text.
type Classifier interface {
	Name() string
	Classify(ctx context.Context, prompt string) (string, error)
}

// ProviderError is a failed provider call. Transient errors (network, 429,
// 5xx) are retried with backoff; others fail the message after max attempts
// like any error, but do not trigger backoff.
type ProviderError struct {
	Provider  string
	Status    int // HTTP status, 0 for transport errors
	Transient bool
	Err       error
}

func (e *ProviderError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s: HTTP %d: %v", e.Provider, e.Status, e.Err)
	}
	return fmt.Sprintf("%s: %v", e.Provider, e.Err)
}

func (e *ProviderError) Unwrap() error { return e.Err }

// IsTransient reports whether err should back off and retry.
func IsTransient(err error) bool {
	var pe *ProviderError
	return errors.As(err, &pe) && pe.Transient
}

// postJSON POSTs body and decodes a 2xx JSON response into out, classifying
// failures as ProviderErrors.
func postJSON(ctx context.Context, client *http.Client, provider, url, token string, body, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return &ProviderError{Provider: provider, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return &ProviderError{Provider: provider, Transient: ctx.Err() == nil, Err: err}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return &ProviderError{
			Provider:  provider,
			Status:    resp.StatusCode,
			Transient: resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
			Err:       errors.New(strings.TrimSpace(string(data))),
		}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &ProviderError{Provider: provider, Status: resp.StatusCode, Err: fmt.Errorf("decode response: %w", err)}
	}
	return nil
}

// ollamaEmbedder calls Ollama /api/embeddings directly.
type ollamaEmbedder struct {
	url, model string
	http       *http.Client
}

// NewOllamaEmbedder returns a direct-Ollama Embedder.
func NewOllamaEmbedder(url, model string) Embedder {
	return &ollamaEmbedder{url: strings.TrimRight(url, "/"), model: model, http: &http.Client{Timeout: 60 * time.Second}}
}

func (o *ollamaEmbedder) Name() string { return "ollama:" + o.model }

func (o *ollamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	var out struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := postJSON(ctx, o.http, o.Name(), o.url+"/api/embeddings", "", map[string]any{"model": o.model, "prompt": text}, &out); err != nil {
		return nil, err
	}
	if len(out.Embedding) == 0 {
		return nil, &ProviderError{Provider: o.Name(), Err: errors.New("empty embedding")}
	}
	return out.Embedding, nil
}

// ollamaClassifier calls Ollama /api/generate directly (non-streaming).
type ollamaClassifier struct {
	url, model string
	http       *http.Client
}

// NewOllamaClassifier returns a direct-Ollama Classifier.
func NewOllamaClassifier(url, model string) Classifier {
	return &ollamaClassifier{url: strings.TrimRight(url, "/"), model: model, http: &http.Client{Timeout: 120 * time.Second}}
}

func (o *ollamaClassifier) Name() string { return "ollama:" + o.model }

func (o *ollamaClassifier) Classify(ctx context.Context, prompt string) (string, error) {
	var out struct {
		Response string `json:"response"`
	}
	err := postJSON(ctx, o.http, o.Name(), o.url+"/api/generate", "", map[string]any{"model": o.model, "prompt": prompt, "stream": false}, &out)
	return out.Response, err
}

// datawatchClassifier routes classification through datawatch's LLM registry
// proxy (POST /api/proxy/llm/<name>), which adds node routing and failover.
// The token needs the sessions:input capability. datawatch reports dispatch
// failures (no node, node down, upstream error) as 502, treated as transient.
type datawatchClassifier struct {
	apiURL, llm, token string
	http               *http.Client
}

// NewDatawatchClassifier returns a Classifier using the datawatch LLM proxy.
func NewDatawatchClassifier(apiURL, llm, token string) Classifier {
	return &datawatchClassifier{apiURL: strings.TrimRight(apiURL, "/"), llm: llm, token: token, http: &http.Client{Timeout: 180 * time.Second}}
}

func (d *datawatchClassifier) Name() string { return "datawatch:" + d.llm }

func (d *datawatchClassifier) Classify(ctx context.Context, prompt string) (string, error) {
	var out struct {
		Text string `json:"text"`
	}
	err := postJSON(ctx, d.http, d.Name(), d.apiURL+"/api/proxy/llm/"+d.llm, d.token, map[string]any{"prompt": prompt}, &out)
	return out.Text, err
}
