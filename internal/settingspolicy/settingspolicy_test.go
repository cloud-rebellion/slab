package settingspolicy

import (
	"strings"
	"testing"
)

func TestValidateCookieDeclarationsJSON_AcceptsEmpty(t *testing.T) {
	if err := validateCookieDeclarationsJSON(""); err != nil {
		t.Errorf("empty value should be accepted (clears the row), got %v", err)
	}
	if err := validateCookieDeclarationsJSON("   "); err != nil {
		t.Errorf("blank value should be accepted, got %v", err)
	}
}

func TestValidateCookieDeclarationsJSON_AcceptsWellFormed(t *testing.T) {
	good := `[{"category":"analytics","name":"_ga","provider":"Google","purpose":"x","expiry":"2 years"}]`
	if err := validateCookieDeclarationsJSON(good); err != nil {
		t.Errorf("expected valid JSON to pass, got %v", err)
	}
}

func TestValidateCookieDeclarationsJSON_RejectsMalformedJSON(t *testing.T) {
	if err := validateCookieDeclarationsJSON("{not json"); err == nil {
		t.Error("expected error on malformed JSON")
	}
}

func TestValidateCookieDeclarationsJSON_RejectsBadCategory(t *testing.T) {
	bad := `[{"category":"made_up","name":"x"}]`
	err := validateCookieDeclarationsJSON(bad)
	if err == nil {
		t.Fatal("expected error on unknown category")
	}
	if !strings.Contains(err.Error(), "category") {
		t.Errorf("error should mention category: %v", err)
	}
}

func TestValidateCookieDeclarationsJSON_RejectsEmptyName(t *testing.T) {
	bad := `[{"category":"analytics","name":"   "}]`
	err := validateCookieDeclarationsJSON(bad)
	if err == nil {
		t.Fatal("expected error on empty name")
	}
}

func TestValidateCookieDeclarationsJSON_RejectsTooManyRows(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < 101; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"category":"analytics","name":"x"}`)
	}
	sb.WriteString("]")
	if err := validateCookieDeclarationsJSON(sb.String()); err == nil {
		t.Error("expected error on >100 rows")
	}
}

func TestValidateCookieDeclarationsJSON_RejectsOversizedField(t *testing.T) {
	long := strings.Repeat("a", 201)
	bad := `[{"category":"analytics","name":"_ga","provider":"` + long + `"}]`
	if err := validateCookieDeclarationsJSON(bad); err == nil {
		t.Error("expected error on >200 char provider")
	}
}

func TestValidateAnalyticsSetting_DispatchesCookieDeclarations(t *testing.T) {
	// Sanity: the analytics dispatcher routes the declarations key to the
	// right validator. Mismatch here means the catalog change was not
	// wired into the handler-level validate switch.
	good := `[{"category":"analytics","name":"_ga"}]`
	if err := validateAnalytics("cookie_declarations", good); err != nil {
		t.Errorf("good declarations rejected by analytics dispatch: %v", err)
	}
	if err := validateAnalytics("cookie_declarations", "{not json"); err == nil {
		t.Error("malformed declarations should fail through analytics dispatch")
	}
}

func TestValidateAnalyticsSetting_PositionEnum(t *testing.T) {
	for _, ok := range []string{"bottom", "top", "center"} {
		if err := validateAnalytics("cookie_banner_position", ok); err != nil {
			t.Errorf("position=%q should be accepted, got %v", ok, err)
		}
	}
	if err := validateAnalytics("cookie_banner_position", "side"); err == nil {
		t.Error("position=side should be rejected")
	}
}

func TestValidateAnalyticsSetting_ThemeEnum(t *testing.T) {
	for _, ok := range []string{"light", "dark", "auto"} {
		if err := validateAnalytics("cookie_theme", ok); err != nil {
			t.Errorf("theme=%q should be accepted, got %v", ok, err)
		}
	}
	if err := validateAnalytics("cookie_theme", "neon"); err == nil {
		t.Error("theme=neon should be rejected")
	}
}

func TestValidateAnalyticsSetting_FloatingTriggerEnum(t *testing.T) {
	for _, ok := range []string{"left", "right", "off"} {
		if err := validateAnalytics("cookie_floating_trigger", ok); err != nil {
			t.Errorf("floating_trigger=%q should be accepted, got %v", ok, err)
		}
	}
	if err := validateAnalytics("cookie_floating_trigger", "top"); err == nil {
		t.Error("floating_trigger=top should be rejected")
	}
}

func TestValidateAnalyticsSetting_ExpiryRange(t *testing.T) {
	if err := validateAnalytics("cookie_expiry_days", "365"); err != nil {
		t.Errorf("365 should be accepted, got %v", err)
	}
	if err := validateAnalytics("cookie_expiry_days", "366"); err == nil {
		t.Error("366 must be rejected: IMY caps at 365")
	}
	if err := validateAnalytics("cookie_expiry_days", "0"); err != nil {
		t.Errorf("0 should be accepted (means use widget default), got %v", err)
	}
	if err := validateAnalytics("cookie_expiry_days", "-1"); err == nil {
		t.Error("negative should be rejected")
	}
}

func TestValidateAnalyticsSetting_TrackPathRelative(t *testing.T) {
	if err := validateAnalytics("track_path", "/t"); err != nil {
		t.Errorf("/t should be accepted, got %v", err)
	}
	if err := validateAnalytics("track_path", "https://evil.com/t"); err == nil {
		t.Error("absolute URL must be rejected (anti-redirect)")
	}
	if err := validateAnalytics("track_path", "//evil.com/t"); err == nil {
		t.Error("protocol-relative URL must be rejected")
	}
	if err := validateAnalytics("track_path", "t"); err == nil {
		t.Error("non-rooted path must be rejected")
	}
}

func TestValidateAnalyticsSetting_CcpaURL(t *testing.T) {
	for _, ok := range []string{"/privacy", "/privacy#ccpa", "https://example.com/do-not-sell"} {
		if err := validateAnalytics("ccpa_url", ok); err != nil {
			t.Errorf("ccpa_url=%q should be accepted, got %v", ok, err)
		}
	}
	if err := validateAnalytics("ccpa_url", "javascript:alert(1)"); err == nil {
		t.Error("javascript: URL must be rejected")
	}
	if err := validateAnalytics("ccpa_url", "ftp://example.com/foo"); err == nil {
		t.Error("non-http(s) absolute URL must be rejected")
	}
}
