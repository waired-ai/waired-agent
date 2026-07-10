package tray

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

func recConnectedSnapshot(rec *management.BenchmarkRecommendation) Snapshot {
	return Snapshot{
		Health: HealthOnline,
		Identity: &management.IdentityView{
			Enrolled: true, AccountEmail: "a@b.c", DeviceID: "dev-1",
			DeviceName: "host", OverlayIP: "100.96.0.10", ControlURL: "https://cp",
		},
		Status: &management.Status{Phase: "active"},
		Catalog: &management.ModelCatalogResponse{
			Active:                  &management.CatalogActive{ModelID: "heavy", DisplayName: "Heavy"},
			BenchmarkRecommendation: rec,
		},
	}
}

func TestUpdate_RecommendationShown(t *testing.T) {
	got := Update(recConnectedSnapshot(&management.BenchmarkRecommendation{
		FromModelID: "heavy", ToModelID: "light", ToVariantID: "q4",
		MeasuredTokps: 10, FloorTokps: 30,
	}))
	if !got.ShowRecommend {
		t.Fatalf("ShowRecommend = false, want true")
	}
	if !strings.Contains(got.RecommendLabel, "light") {
		t.Errorf("RecommendLabel = %q, want it to mention the target model", got.RecommendLabel)
	}
}

func TestUpdate_RecommendationDismissedHidden(t *testing.T) {
	got := Update(recConnectedSnapshot(&management.BenchmarkRecommendation{
		FromModelID: "heavy", ToModelID: "light", ToVariantID: "q4", Dismissed: true,
	}))
	if got.ShowRecommend {
		t.Errorf("ShowRecommend = true, want false (dismissed)")
	}
}

func TestUpdate_RecommendationNoneHidden(t *testing.T) {
	got := Update(recConnectedSnapshot(nil))
	if got.ShowRecommend {
		t.Errorf("ShowRecommend = true, want false (no recommendation)")
	}
}

func upgradeConnectedSnapshot(rec *management.BenchmarkRecommendation) Snapshot {
	s := recConnectedSnapshot(nil)
	s.Catalog.BenchmarkUpgrade = rec
	return s
}

func TestUpdate_UpgradeShown(t *testing.T) {
	got := Update(upgradeConnectedSnapshot(&management.BenchmarkRecommendation{
		Direction:   management.RecommendationUpgrade,
		FromModelID: "light", ToModelID: "heavy", ToVariantID: "q4",
		MeasuredTokps: 101, FloorTokps: 30, PredictedTokps: 236,
	}))
	if !got.ShowRecommend {
		t.Fatalf("ShowRecommend = false, want true")
	}
	if !strings.Contains(got.RecommendLabel, "heavy") || !strings.Contains(got.RecommendLabel, "Better model") {
		t.Errorf("RecommendLabel = %q, want an upgrade-flavoured label naming heavy", got.RecommendLabel)
	}
}

func TestUpdate_UpgradeDismissedHidden(t *testing.T) {
	got := Update(upgradeConnectedSnapshot(&management.BenchmarkRecommendation{
		Direction: management.RecommendationUpgrade,
		ToModelID: "heavy", ToVariantID: "q4", Dismissed: true,
	}))
	if got.ShowRecommend {
		t.Errorf("ShowRecommend = true, want false (dismissed upgrade)")
	}
}
