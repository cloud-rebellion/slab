package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/shield"
)

// Audit 2026-08-11 finding 12, second half: handlePromptsGet swallowed the
// error from Session.RedactString and kept the UNREDACTED text, while its two
// siblings (handleToolsCall, handleResourcesRead) explicitly withhold. Its own
// comment described the fail-closed behaviour it did not have.
//
// A tokenization failure is exactly when the raw prompt, PII included, must
// NOT reach the model.
func TestPromptsGetFailsClosedOnRedactError(t *testing.T) {
	store := &promptBrokenStore{MemoryStore: shield.NewMemoryStore()}
	srv := NewServer(nil, nil)
	srv.WithShield(store, []byte("0123456789abcdef0123456789abcdef"), time.Hour, shield.HintFull)

	srv.prompts["leaky"] = Prompt{
		Name:        "leaky",
		Description: "returns text carrying PII",
		Render: func(ctx context.Context, a *authmw.AgentIdentity, args map[string]string) ([]PromptMessage, error) {
			return []PromptMessage{{
				Role:    "user",
				Content: Content{Type: "text", Text: "Follow up with anna@andersson-law.se about the invoice."},
			}}, nil
		},
	}

	r := httptest.NewRequest(http.MethodPost, "/", nil)
	params, err := json.Marshal(map[string]string{"name": "leaky"})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}

	res, rerr := srv.handlePromptsGet(r, testAgent, params)
	if rerr == nil {
		out, _ := json.Marshal(res)
		t.Fatalf("prompts/get returned a result when redaction failed; body=%s", out)
	}
	if res != nil {
		out, _ := json.Marshal(res)
		if strings.Contains(string(out), "anna@andersson-law.se") {
			t.Fatalf("PII forwarded alongside the error: %s", out)
		}
	}
	if !strings.Contains(rerr.Message, "withheld") {
		t.Errorf("error message = %q, want it to say the prompt was withheld", rerr.Message)
	}
}

// TestPromptsGetRedactsOnHappyPath keeps the fail-closed change honest: a
// working store must still return a redacted prompt rather than an error.
func TestPromptsGetRedactsOnHappyPath(t *testing.T) {
	srv := NewServer(nil, nil)
	srv.WithShield(shield.NewMemoryStore(), []byte("0123456789abcdef0123456789abcdef"), time.Hour, shield.HintFull)

	srv.prompts["leaky"] = Prompt{
		Name: "leaky",
		Render: func(ctx context.Context, a *authmw.AgentIdentity, args map[string]string) ([]PromptMessage, error) {
			return []PromptMessage{{
				Role:    "user",
				Content: Content{Type: "text", Text: "Follow up with anna@andersson-law.se about the invoice."},
			}}, nil
		},
	}

	r := httptest.NewRequest(http.MethodPost, "/", nil)
	params, err := json.Marshal(map[string]string{"name": "leaky"})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	res, rerr := srv.handlePromptsGet(r, testAgent, params)
	if rerr != nil {
		t.Fatalf("unexpected error: %v", rerr.Message)
	}
	out, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if strings.Contains(string(out), "anna@andersson-law.se") {
		t.Errorf("email survived the redact pass: %s", out)
	}
	if !strings.Contains(string(out), "[shield:email:") {
		t.Errorf("no marker in the rendered prompt: %s", out)
	}
}

type promptBrokenStore struct {
	*shield.MemoryStore
}

func (b *promptBrokenStore) InsertToken(ctx context.Context, sessionID, token, kind, hint, ciphertext string) error {
	return errors.New("store unavailable")
}
