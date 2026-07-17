package domains

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// CloudflareClient is a tiny wrapper over the Cloudflare API v4 surface
// for the only thing atomicsite needs: "ensure an A record exists for
// hostname pointing at edgeIP". We deliberately do not pull in
// cloudflare-go because the surface needed is one POST and one GET.
//
// When an admin attaches a custom hostname whose zone we control (the
// reconciler matches the longest-suffix zone in CloudflareClient.Zones),
// we call EnsureA(hostname, edgeIP) so DNS propagation kicks off the
// instant they hit Save. Customers whose domain we don't control fall
// through to the manual "point an A record at <edge>" path.
//
// Auth: a scoped Cloudflare API token with Zone.DNS:Edit on the listed
// zones. Tokens are stored as Dockyard vault entries; the reconciler
// reads the token via env var (ATOMICSITE_CLOUDFLARE_TOKEN) so vault
// rotation hot-swaps without a restart.
type CloudflareClient struct {
	APIToken string
	// Zones maps a zone apex (e.g. "brightinteraction.com") to its
	// Cloudflare zone ID. Configured at boot; populated from env or
	// from a Dockyard /vault export. Empty map disables the helper.
	Zones map[string]string
	// HTTPClient lets tests inject a mock. Defaults to a 10s client.
	HTTPClient *http.Client
}

// Enabled reports whether the helper has enough to make calls.
func (c *CloudflareClient) Enabled() bool {
	return c != nil && c.APIToken != "" && len(c.Zones) > 0
}

func (c *CloudflareClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// MatchZone returns the zone ID and zone apex for a hostname when the
// hostname falls under any configured zone. Picks the longest matching
// suffix so "blog.example.com" matches "example.com" rather than a less
// specific parent. Returns "" when no zone matches.
func (c *CloudflareClient) MatchZone(hostname string) (zoneID, zoneApex string) {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(hostname), "."))
	bestApex, bestID := "", ""
	for apex, id := range c.Zones {
		a := strings.ToLower(strings.TrimSuffix(apex, "."))
		if host == a || strings.HasSuffix(host, "."+a) {
			if len(a) > len(bestApex) {
				bestApex, bestID = a, id
			}
		}
	}
	return bestID, bestApex
}

// EnsureA creates or updates an A record for hostname pointing at
// edgeIP inside the matching Cloudflare zone. Idempotent: if a record
// already exists with the right content, we PUT to update; otherwise
// POST. Proxied=false because we want LE HTTP-01 to hit our IP, not
// the Cloudflare proxy (admin can flip the orange cloud later).
func (c *CloudflareClient) EnsureA(ctx context.Context, hostname, edgeIP string) error {
	if !c.Enabled() {
		return fmt.Errorf("cloudflare client not configured")
	}
	zoneID, _ := c.MatchZone(hostname)
	if zoneID == "" {
		return fmt.Errorf("no Cloudflare zone matches %q", hostname)
	}
	if edgeIP == "" {
		return fmt.Errorf("edge IP is empty")
	}

	existing, err := c.listRecords(ctx, zoneID, hostname)
	if err != nil {
		return err
	}
	for _, r := range existing {
		if r.Type == "A" && strings.EqualFold(r.Name, hostname) {
			if r.Content == edgeIP {
				return nil
			}
			// Only ever overwrite a record atomicsite itself created (createRecord
			// tags them). A pre-existing UNMANAGED A record - an operator service
			// (auth/crm/svar) or a co-tenant's externally-managed name that happens
			// to sit in this zone - must NOT be repointed at our edge, or any tenant
			// that adds that hostname as a custom domain would hijack it before any
			// ownership proof. Refuse, so the reconciler marks the domain error and
			// the DNS/verify/cert chain never proceeds.
			if r.Comment != managedComment {
				return fmt.Errorf("hostname %s already resolves to an unmanaged DNS record; refusing to overwrite", hostname)
			}
			return c.updateRecord(ctx, zoneID, r.ID, hostname, edgeIP)
		}
	}
	return c.createRecord(ctx, zoneID, hostname, edgeIP)
}

// managedComment tags the A records atomicsite provisions, so EnsureA can tell
// its own records apart from pre-existing operator/co-tenant infrastructure and
// never overwrite the latter.
const managedComment = "managed by atomicsite"

type cfRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Comment string `json:"comment"`
}

type cfListResp struct {
	Success bool       `json:"success"`
	Errors  []cfErr    `json:"errors"`
	Result  []cfRecord `json:"result"`
}

type cfWriteResp struct {
	Success bool     `json:"success"`
	Errors  []cfErr  `json:"errors"`
	Result  cfRecord `json:"result"`
}

type cfErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *CloudflareClient) listRecords(ctx context.Context, zoneID, name string) ([]cfRecord, error) {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?type=A&name=%s", zoneID, name)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var parsed cfListResp
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	if !parsed.Success {
		return nil, fmt.Errorf("cloudflare list: %s", joinErrs(parsed.Errors))
	}
	return parsed.Result, nil
}

func (c *CloudflareClient) createRecord(ctx context.Context, zoneID, hostname, ip string) error {
	body, _ := json.Marshal(map[string]any{
		"type":    "A",
		"name":    hostname,
		"content": ip,
		"proxied": false,
		"ttl":     300,
		"comment": managedComment,
	})
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", zoneID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var parsed cfWriteResp
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return err
	}
	if !parsed.Success {
		return fmt.Errorf("cloudflare create: %s", joinErrs(parsed.Errors))
	}
	return nil
}

func (c *CloudflareClient) updateRecord(ctx context.Context, zoneID, recordID, hostname, ip string) error {
	body, _ := json.Marshal(map[string]any{
		"type":    "A",
		"name":    hostname,
		"content": ip,
		"proxied": false,
		"ttl":     300,
	})
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", zoneID, recordID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var parsed cfWriteResp
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return err
	}
	if !parsed.Success {
		return fmt.Errorf("cloudflare update: %s", joinErrs(parsed.Errors))
	}
	return nil
}

func joinErrs(errs []cfErr) string {
	if len(errs) == 0 {
		return "(no detail)"
	}
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, fmt.Sprintf("%d %s", e.Code, e.Message))
	}
	return strings.Join(parts, "; ")
}
