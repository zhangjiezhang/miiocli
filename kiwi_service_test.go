package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v2"
)

func TestKiwiServiceRefreshesServiceInfo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("veid"); got != "12345" {
			t.Errorf("veid = %q, want %q", got, "12345")
		}
		if got := r.URL.Query().Get("api_key"); got != "secret" {
			t.Errorf("api_key = %q, want %q", got, "secret")
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error":                   false,
			"hostname":                "vm01",
			"node_location":           "Los Angeles, US",
			"plan":                    "test-plan",
			"os":                      "debian",
			"ip_addresses":            []string{"192.0.2.1", "2001:db8::1"},
			"plan_disk":               "10737418240",
			"plan_ram":                536870912,
			"plan_swap":               268435456,
			"monthly_data_multiplier": 2,
			"plan_monthly_data":       1099511627776,
			"data_counter":            274877906944,
			"data_next_reset":         1788211200,
			"suspended":               0,
			"policy_violation":        false,
		})
	}))
	defer server.Close()

	service := newKiwiService("12345", "secret", server.URL, server.Client())
	service.refresh(context.Background())
	data := service.Snapshot()

	if !data.Configured || !data.Healthy || data.Error != "" {
		t.Fatalf("unexpected state: %+v", data)
	}
	if data.Hostname != "vm01" || data.NodeLocation != "Los Angeles, US" {
		t.Fatalf("unexpected server identity: %+v", data)
	}
	if data.PlanDiskBytes != 10*1024*1024*1024 {
		t.Errorf("plan_disk_bytes = %d", data.PlanDiskBytes)
	}
	if data.MonthlyTrafficBytes != 2*1099511627776 {
		t.Errorf("monthly_traffic_bytes = %f", data.MonthlyTrafficBytes)
	}
	if data.UsedTrafficBytes != 2*274877906944 {
		t.Errorf("used_traffic_bytes = %f", data.UsedTrafficBytes)
	}
	if math.Abs(data.UsedPercent-25) > 0.001 {
		t.Errorf("used_percent = %f, want 25", data.UsedPercent)
	}
	if got := data.ResetTimeChina.Format("2006-01-02 15:04:05 -0700"); got != "2026-09-01 05:20:00 +0800" {
		t.Errorf("reset_time_china = %s", got)
	}
}

func TestKiwiServiceKeepsLastDataAfterRefreshFailure(t *testing.T) {
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "unavailable", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"hostname":"vm01"}`))
	}))
	defer server.Close()

	service := newKiwiService("12345", "secret", server.URL, server.Client())
	service.refresh(context.Background())
	firstUpdatedAt := service.Snapshot().UpdatedAt
	fail = true
	time.Sleep(time.Millisecond)
	service.refresh(context.Background())
	data := service.Snapshot()

	if data.Healthy {
		t.Fatal("service should be unhealthy after a failed refresh")
	}
	if data.Hostname != "vm01" || !data.UpdatedAt.Equal(firstUpdatedAt) {
		t.Fatalf("last successful data was not retained: %+v", data)
	}
	if data.Error == "" || !data.LastAttempt.After(firstUpdatedAt) {
		t.Fatalf("failure state was not recorded: %+v", data)
	}
}

func TestKiwiServiceReportsMissingConfiguration(t *testing.T) {
	data := NewKiwiService(KiwiConfig{}).Snapshot()
	if data.Configured || data.Error == "" {
		t.Fatalf("unexpected state: %+v", data)
	}
}

func TestKiwiConfigurationIsReadFromYAML(t *testing.T) {
	var parsed Config
	if err := yaml.Unmarshal([]byte("kiwi:\n  veid: 12345\n  apiKey: secret\n  intervalSeconds: 45\n"), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Kiwi.VEID != "12345" || parsed.Kiwi.APIKey != "secret" {
		t.Fatalf("unexpected Kiwi configuration: %+v", parsed.Kiwi)
	}
	if parsed.Kiwi.PollInterval() != 45*time.Second {
		t.Fatalf("poll interval = %s, want 45s", parsed.Kiwi.PollInterval())
	}
	if (KiwiConfig{}).PollInterval() != 30*time.Second {
		t.Fatalf("default poll interval = %s, want 30s", (KiwiConfig{}).PollInterval())
	}
}

func TestStaticResponseIncludesKiwiData(t *testing.T) {
	previousService := kiwiService
	previousResult := resultData
	previousConfig := config
	t.Cleanup(func() {
		kiwiService = previousService
		resultData = previousResult
		config = previousConfig
	})

	kiwiService = NewKiwiService(KiwiConfig{VEID: "12345", APIKey: "secret"})
	kiwiService.data = KiwiData{
		Configured:          true,
		Healthy:             true,
		PollIntervalSeconds: 30,
		Hostname:            "vm01",
	}
	resultData = ResultData{
		Powers:       map[string]float64{},
		Temperatures: map[string]float64{},
	}
	config = Config{}

	response := httptest.NewRecorder()
	static(response, httptest.NewRequest(http.MethodGet, "/static", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	for _, expected := range []string{`"kiwi"`, `"hostname":"vm01"`, `"poll_interval_seconds":30`} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Errorf("response does not contain %s: %s", expected, response.Body.String())
		}
	}
}
