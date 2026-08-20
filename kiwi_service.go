package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	kiwiServiceInfoURL      = "https://api.64clouds.com/v1/getServiceInfo"
	defaultKiwiPollInterval = 30 * time.Second
)

type KiwiData struct {
	Configured          bool                   `json:"configured"`
	Healthy             bool                   `json:"healthy"`
	PollIntervalSeconds int                    `json:"poll_interval_seconds"`
	Error               string                 `json:"error,omitempty"`
	LastAttempt         time.Time              `json:"last_attempt,omitempty"`
	UpdatedAt           time.Time              `json:"updated_at,omitempty"`
	Hostname            string                 `json:"hostname,omitempty"`
	NodeLocation        string                 `json:"node_location,omitempty"`
	Plan                string                 `json:"plan,omitempty"`
	OS                  string                 `json:"os,omitempty"`
	IPAddresses         []string               `json:"ip_addresses,omitempty"`
	PlanDiskBytes       int64                  `json:"plan_disk_bytes"`
	PlanRAMBytes        int64                  `json:"plan_ram_bytes"`
	PlanSwapBytes       int64                  `json:"plan_swap_bytes"`
	MonthlyTrafficBytes float64                `json:"monthly_traffic_bytes"`
	UsedTrafficBytes    float64                `json:"used_traffic_bytes"`
	UsedPercent         float64                `json:"used_percent"`
	DataNextReset       int64                  `json:"data_next_reset"`
	ResetTimeUTC        time.Time              `json:"reset_time_utc,omitempty"`
	ResetTimeChina      time.Time              `json:"reset_time_china,omitempty"`
	Suspended           bool                   `json:"suspended"`
	PolicyViolation     bool                   `json:"policy_violation"`
	Raw                 map[string]interface{} `json:"raw,omitempty"`
}

type KiwiConfig struct {
	VEID            string `yaml:"veid"`
	APIKey          string `yaml:"apiKey"`
	IntervalSeconds int    `yaml:"intervalSeconds"`
}

func (c KiwiConfig) PollInterval() time.Duration {
	if c.IntervalSeconds <= 0 {
		return defaultKiwiPollInterval
	}
	return time.Duration(c.IntervalSeconds) * time.Second
}

type KiwiService struct {
	mu       sync.RWMutex
	client   *http.Client
	endpoint string
	veid     string
	apiKey   string
	data     KiwiData
}

func NewKiwiService(config KiwiConfig) *KiwiService {
	service := newKiwiService(config.VEID, config.APIKey, kiwiServiceInfoURL, &http.Client{Timeout: 15 * time.Second})
	service.data.PollIntervalSeconds = int(config.PollInterval() / time.Second)
	return service
}

func newKiwiService(veid, apiKey, endpoint string, client *http.Client) *KiwiService {
	configured := strings.TrimSpace(veid) != "" && strings.TrimSpace(apiKey) != ""
	data := KiwiData{Configured: configured}
	if !configured {
		data.Error = "未配置 kiwi.veid 或 kiwi.apiKey"
	}
	return &KiwiService{
		client:   client,
		endpoint: endpoint,
		veid:     strings.TrimSpace(veid),
		apiKey:   strings.TrimSpace(apiKey),
		data:     data,
	}
}

func (s *KiwiService) Run(ctx context.Context, interval time.Duration) {
	if !s.Snapshot().Configured {
		return
	}
	s.mu.Lock()
	s.data.PollIntervalSeconds = int(interval / time.Second)
	s.mu.Unlock()
	s.refresh(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refresh(ctx)
		}
	}
}

func (s *KiwiService) refresh(ctx context.Context) {
	now := time.Now()
	data, err := s.fetch(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.data.Healthy = false
		s.data.Error = err.Error()
		s.data.LastAttempt = now
		return
	}
	data.Configured = true
	data.Healthy = true
	data.PollIntervalSeconds = s.data.PollIntervalSeconds
	data.LastAttempt = now
	data.UpdatedAt = now
	s.data = data
}

func (s *KiwiService) fetch(ctx context.Context) (KiwiData, error) {
	endpoint, err := url.Parse(s.endpoint)
	if err != nil {
		return KiwiData{}, fmt.Errorf("解析 KiwiVM API 地址失败: %w", err)
	}
	query := endpoint.Query()
	query.Set("veid", s.veid)
	query.Set("api_key", s.apiKey)
	endpoint.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return KiwiData{}, fmt.Errorf("创建 KiwiVM API 请求失败: %w", err)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return KiwiData{}, fmt.Errorf("调用 KiwiVM API 失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return KiwiData{}, fmt.Errorf("KiwiVM API 返回 HTTP %d", response.StatusCode)
	}

	decoder := json.NewDecoder(io.LimitReader(response.Body, 2<<20))
	decoder.UseNumber()
	var raw map[string]interface{}
	if err := decoder.Decode(&raw); err != nil {
		return KiwiData{}, fmt.Errorf("解析 KiwiVM API 响应失败: %w", err)
	}
	if apiError, exists := raw["error"]; exists && valueBool(apiError) {
		return KiwiData{}, fmt.Errorf("KiwiVM API 错误: %s", strings.TrimSpace(valueString(apiError)))
	}

	multiplier := valueFloat(raw["monthly_data_multiplier"])
	if multiplier == 0 {
		multiplier = 1
	}
	monthlyTraffic := valueFloat(raw["plan_monthly_data"]) * multiplier
	usedTraffic := valueFloat(raw["data_counter"]) * multiplier
	usedPercent := float64(0)
	if monthlyTraffic > 0 {
		usedPercent = usedTraffic / monthlyTraffic * 100
	}
	nextReset := valueInt64(raw["data_next_reset"])
	var resetUTC, resetChina time.Time
	if nextReset > 0 {
		resetUTC = time.Unix(nextReset, 0).UTC()
		resetChina = resetUTC.In(time.FixedZone("UTC+8", 8*60*60))
	}

	return KiwiData{
		Hostname:            valueString(raw["hostname"]),
		NodeLocation:        valueString(raw["node_location"]),
		Plan:                valueString(raw["plan"]),
		OS:                  valueString(raw["os"]),
		IPAddresses:         valueStrings(raw["ip_addresses"]),
		PlanDiskBytes:       valueInt64(raw["plan_disk"]),
		PlanRAMBytes:        valueInt64(raw["plan_ram"]),
		PlanSwapBytes:       valueInt64(raw["plan_swap"]),
		MonthlyTrafficBytes: monthlyTraffic,
		UsedTrafficBytes:    usedTraffic,
		UsedPercent:         usedPercent,
		DataNextReset:       nextReset,
		ResetTimeUTC:        resetUTC,
		ResetTimeChina:      resetChina,
		Suspended:           valueBool(raw["suspended"]),
		PolicyViolation:     valueBool(raw["policy_violation"]),
		Raw:                 raw,
	}, nil
}

func (s *KiwiService) Snapshot() KiwiData {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data := s.data
	data.IPAddresses = append([]string(nil), s.data.IPAddresses...)
	data.Raw = copyMap(s.data.Raw)
	return data
}

func valueString(value interface{}) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func valueFloat(value interface{}) float64 {
	switch number := value.(type) {
	case json.Number:
		result, _ := number.Float64()
		return result
	case float64:
		return number
	case string:
		result, _ := strconv.ParseFloat(number, 64)
		return result
	default:
		return 0
	}
}

func valueInt64(value interface{}) int64 {
	return int64(valueFloat(value))
}

func valueBool(value interface{}) bool {
	switch item := value.(type) {
	case bool:
		return item
	case string:
		result, err := strconv.ParseBool(item)
		if err == nil {
			return result
		}
		return item != "" && item != "0"
	default:
		return valueFloat(value) != 0
	}
}

func valueStrings(value interface{}) []string {
	items, ok := value.([]interface{})
	if !ok {
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		result = append(result, valueString(item))
	}
	return result
}
