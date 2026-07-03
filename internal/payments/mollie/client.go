// Package mollie wraps the per-tenant subset of the Mollie REST API
// (https://docs.mollie.com) needed for storefront checkout. This is
// distinct from internal/cloud/mollie which is the EE-only Atomic
// Site SaaS billing wrapper; this package is OSS-buildable (no ee
// build tag) because tenants of any tier can sell on their own
// storefront.
//
// Auth is per-tenant: each site stores its own test_xxx or live_xxx
// API key in site_settings under category='payments', key=
// 'mollie_api_key'. The handler reads + constructs a Client per
// request rather than keeping a pool; Mollie's rate limits are
// per-API-key so this naturally isolates tenants.
//
// Sprint 2 slice B of the WP/Webflow replacement roadmap.

package mollie

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

const APIBase = "https://api.mollie.com/v2"

type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

func NewClient(apiKey string) *Client {
	return &Client{
		apiKey:  apiKey,
		baseURL: APIBase,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) WithBaseURL(u string) *Client {
	c.baseURL = u
	return c
}

func (c *Client) IsConfigured() bool { return c.apiKey != "" }

func (c *Client) Mode() string {
	switch {
	case strings.HasPrefix(c.apiKey, "test_"):
		return "test"
	case strings.HasPrefix(c.apiKey, "live_"):
		return "live"
	default:
		return ""
	}
}

// MoneyValue is Mollie's amount shape: a currency code + a string
// value with two decimal places, e.g. {"currency":"EUR","value":"29.95"}.
type MoneyValue struct {
	Currency string `json:"currency"`
	Value    string `json:"value"`
}

type PaymentLinks struct {
	Checkout *LinkRef `json:"checkout,omitempty"`
	Self     *LinkRef `json:"self,omitempty"`
}

type LinkRef struct {
	Href string `json:"href"`
	Type string `json:"type"`
}

type Payment struct {
	ID           string         `json:"id"`
	Status       string         `json:"status"`
	Amount       MoneyValue     `json:"amount"`
	Description  string         `json:"description"`
	RedirectURL  string         `json:"redirectUrl"`
	WebhookURL   string         `json:"webhookUrl,omitempty"`
	Method       string         `json:"method,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	Links        PaymentLinks   `json:"_links"`
	CreatedAt    string         `json:"createdAt,omitempty"`
	PaidAt       string         `json:"paidAt,omitempty"`
	CanceledAt   string         `json:"canceledAt,omitempty"`
	ExpiredAt    string         `json:"expiredAt,omitempty"`
	FailedAt     string         `json:"failedAt,omitempty"`
	IsCancelable bool           `json:"isCancelable,omitempty"`
}

type CreatePaymentInput struct {
	Amount      MoneyValue     `json:"amount"`
	Description string         `json:"description"`
	RedirectURL string         `json:"redirectUrl"`
	WebhookURL  string         `json:"webhookUrl,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Method      string         `json:"method,omitempty"`
	Locale      string         `json:"locale,omitempty"`
}

func (c *Client) CreatePayment(ctx context.Context, in CreatePaymentInput) (*Payment, error) {
	if !c.IsConfigured() {
		return nil, errors.New("mollie: API key not configured for this site")
	}
	var out Payment
	if err := c.do(ctx, "POST", "/payments", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetPayment(ctx context.Context, id string) (*Payment, error) {
	if !c.IsConfigured() {
		return nil, errors.New("mollie: API key not configured")
	}
	var out Payment
	if err := c.do(ctx, "GET", "/payments/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type Refund struct {
	ID          string     `json:"id"`
	Amount      MoneyValue `json:"amount"`
	Status      string     `json:"status"`
	PaymentID   string     `json:"paymentId"`
	Description string     `json:"description"`
	CreatedAt   string     `json:"createdAt"`
}

type CreateRefundInput struct {
	Amount      MoneyValue `json:"amount"`
	Description string     `json:"description,omitempty"`
}

func (c *Client) CreateRefund(ctx context.Context, paymentID string, in CreateRefundInput) (*Refund, error) {
	if !c.IsConfigured() {
		return nil, errors.New("mollie: API key not configured")
	}
	var out Refund
	if err := c.do(ctx, "POST", "/payments/"+paymentID+"/refunds", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FormatAmount turns integer cents into Mollie's two-decimal string.
func FormatAmount(cents int64) string {
	if cents < 0 {
		return fmt.Sprintf("-%d.%02d", -cents/100, -cents%100)
	}
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("mollie %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("mollie %s %s: status %d body=%s", method, path, resp.StatusCode, string(respBody))
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	return json.Unmarshal(respBody, out)
}
