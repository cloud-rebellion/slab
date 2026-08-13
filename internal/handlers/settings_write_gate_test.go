package handlers

import (
	"os"
	"strings"
	"testing"
)

// agentReachableSettingsWriters are the source files holding a settings
// write that an AGENT credential can reach. Both must gate on the
// {category, key} pair, never on the category alone.
//
// The human REST surface (settings.go) is deliberately not in this list:
// it has its own predicate, settingsWriteNeedsOwner, which already
// combines PrivilegedCategory with IsSecretValue and is covered by
// authz_gates_test.go.
var agentReachableSettingsWriters = map[string]string{
	"agent_settings.go": "agentWritableSetting",
	"../mcp/tools.go":   "isAgentWritableSetting",
}

// categoryOnlyGates are the helper names that take a category and nothing
// else. Calling one of these to authorize a WRITE is the defect: analytics
// is agent-writable and analytics.crm_webhook_secret is a secret, so a
// category-only gate let an agent overwrite a credential that the very
// same surface masks on read.
var categoryOnlyGates = []string{
	"isAgentWritableCategory(",
	"agentWritableSettingsCategories(",
}

// TestAgentSettingsWritesGateOnCategoryAndKey enumerates the agent-reachable
// settings writers and asserts each uses the pair-aware gate and none uses a
// category-only one.
//
// This exists because the three bulk settings surfaces were harmonised on
// their envelope SHAPE while their authorization PREDICATE silently
// diverged, which reads as consistency at a glance. A source-level check is
// the cheap way to stop surface four appearing with the weaker gate.
func TestAgentSettingsWritesGateOnCategoryAndKey(t *testing.T) {
	for file, wantGate := range agentReachableSettingsWriters {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		body := string(src)

		if !strings.Contains(body, wantGate+"(") {
			t.Errorf("%s: expected the pair-aware gate %s( to authorize its settings writes, found none", file, wantGate)
		}
		for _, bad := range categoryOnlyGates {
			if strings.Contains(body, bad) {
				t.Errorf("%s: calls category-only gate %s to authorize a settings write; "+
					"use %s(category, key) so a secret-valued key inside a writable category is refused",
					file, bad, wantGate)
			}
		}
	}
}

// TestSettingsWriteGateFixtureIsHonest is the anti-vacuity guard. If the
// files move or the helper is renamed, the test above would pass by
// checking nothing, so assert the fixture still points at real files that
// really do perform a settings write.
func TestSettingsWriteGateFixtureIsHonest(t *testing.T) {
	if len(agentReachableSettingsWriters) == 0 {
		t.Fatal("fixture is empty, so the gate test checks nothing")
	}
	for file := range agentReachableSettingsWriters {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if !strings.Contains(string(src), "UpsertSetting(") {
			t.Errorf("%s no longer contains a settings write, so listing it proves nothing; "+
				"either drop it or point the fixture at wherever the write moved", file)
		}
	}
}
