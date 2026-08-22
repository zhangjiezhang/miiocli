package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	miPlugPower = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "mi_plug_power",
			Help: "小米智能插座功耗",
		},
		[]string{"name", "alias"},
	)
	miPlugTemperature = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "mi_plug_temperature",
			Help: "小米智能插座温度",
		},
		[]string{"name", "alias"},
	)
	esxiTemperature = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vmware_host_sensor_power_watt",
			Help: "小米智能插座温度在vmware中统计",
			ConstLabels: map[string]string{
				"dc_name": "ha-datacenter",
				"name":    "System Board 1 Pwr Consumption",
			},
		},
		[]string{"host_name", "alias"},
	)
	filePath     = ""
	daily        float64
	config       Config
	voiceHub     *VoiceHub
	otaService   *OTAService
	webService   *WebReleaseService
	codexService *CodexSessionService
	kiwiService  *KiwiService
	resultData   ResultData
	resultMu     sync.RWMutex
)

type Config struct {
	TrafficAddress string           `yaml:"trafficAddress"`
	Mis            []Mi             `yaml:"mis"`
	Voice          VoiceConfig      `yaml:"voice"`
	OTA            OTAConfig        `yaml:"ota"`
	WebApp         WebReleaseConfig `yaml:"webApp"`
	Kiwi           KiwiConfig       `yaml:"kiwi"`
}
type Mi struct {
	Name     string `yaml:"name"`
	Alias    string `yaml:"alias"`
	Sort     int    `yaml:"sort"`
	Ip       string `yaml:"ip"`
	Token    string `yaml:"token"`
	Drive    string `yaml:"drive"`
	HostName string `yaml:"hostName"`
	Asyn     bool   `yaml:"asyn"`
}

// [{'did': '11-2', 'siid': 11, 'piid': 2, 'code': 0, 'value': 508}]
type Miio struct {
	Did   string  `yaml:"did"`
	Siid  int     `yaml:"siid"`
	Piid  int     `yaml:"piid"`
	Code  int     `yaml:"code"`
	Value float64 `yaml:"value"`
}

type ResultData struct {
	Powers       map[string]float64     `json:"powers"`
	Temperatures map[string]float64     `json:"temperatures"`
	Traffic      map[string]interface{} `json:"traffic"`
	Devices      []DeviceData           `json:"devices,omitempty"`
	Kiwi         *KiwiData              `json:"kiwi,omitempty"`
}
type DeviceData struct {
	Name        string   `json:"name"`
	Alias       string   `json:"alias"`
	Sort        int      `json:"sort"`
	Power       *float64 `json:"power,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
}
type Result struct {
	Code int        `json:"code"`
	Msg  string     `json:"msg"`
	Data ResultData `json:"data"`
}

//go:embed dashboard.html
var dashboardHTML string

//go:embed ota.html
var otaHTML string

//go:embed webapp.html
var webAppHTML string

//go:embed websocket.html
var websocketHTML string

//go:embed codex.html
var codexHTML string

//go:embed app.css
var appCSS string

func main() {
	flag.StringVar(&filePath, "filePath", "./app.yaml", "config file path")
	flag.Float64Var(&daily, "daily", 10, "daily seconds")
	flag.Parse()
	if len(filePath) == 0 {
		log.Fatalf("param is null")
		return
	}
	file, err := os.ReadFile(filePath)
	if err != nil {
		log.Fatalf("读取配置文件失败 #%v", err)
		return
	}
	err = yaml.Unmarshal(file, &config)
	if err != nil {
		log.Fatalf("解析失败: %v", err)
		return
	}
	//prometheus.MustRegister(miPlugPower)
	//prometheus.MustRegister(miPlugTemperature)
	mis := config.Mis
	resultData.Powers = make(map[string]float64, len(mis))
	resultData.Temperatures = make(map[string]float64, len(mis))
	kiwiService = NewKiwiService(config.Kiwi)
	if kiwiService.Snapshot().Configured {
		go kiwiService.Run(context.Background(), config.Kiwi.PollInterval())
	} else {
		log.Printf("KiwiVM service disabled: kiwi.veid or kiwi.apiKey is not configured")
	}

	callEndpoint()
	voiceHub = NewVoiceHub(config.Voice)
	otaService, err = NewOTAService(config.OTA, config.Voice.Devices)
	if err != nil {
		log.Printf("OTA service disabled: %v", err)
	}
	webService, err = NewWebReleaseService(config.WebApp)
	if err != nil {
		log.Printf("web release service disabled: %v", err)
	}
	ttsService, err := NewAliyunTTSService(config.Voice.TTS, config.Voice.APIToken, voiceHub)
	if err != nil {
		log.Printf("voice TTS endpoint disabled: %v", err)
	}
	codexService, err = NewCodexSessionService(config.Voice.Codex, config.Voice.APIToken,
		config.Voice.Devices)
	if err != nil {
		log.Printf("Codex session integration disabled: %v", err)
	} else {
		voiceHub.SetCodexSessionHandler(func(deviceID, action, sessionID string) {
			switch action {
			case "session_list":
				_ = voiceHub.SendCodexSessions(deviceID, codexService.DeviceSessionOptions(deviceID))
			case "session_next":
				selected, selectErr := codexService.SelectNextDeviceSession(deviceID)
				if selectErr != nil {
					_ = voiceHub.SendCommandState(deviceID, "", "error", selectErr.Error(), "")
					return
				}
				_ = voiceHub.SendCodexSessionSelected(deviceID, selected)
			case "session_select":
				selected, selectErr := codexService.SelectDeviceSession(deviceID, sessionID)
				if selectErr != nil {
					_ = voiceHub.SendCommandState(deviceID, "", "error", selectErr.Error(), "")
					return
				}
				_ = voiceHub.SendCodexSessionSelected(deviceID, selected)
			}
		})
	}
	commandService, err := NewVoiceCommandService(config.Voice, voiceHub, ttsService, codexService)
	if err != nil {
		log.Printf("voice command service disabled: %v", err)
	} else {
		voiceHub.SetUtteranceHandler(commandService.HandleUtterance)
	}
	go func() {
		for {
			callTraffic(config.TrafficAddress)
			time.Sleep(time.Duration(1) * time.Second)
		}
	}()

	http.Handle("/metrics", http.HandlerFunc(metrics))
	http.Handle("/static", http.HandlerFunc(static))
	http.Handle("/app.css", http.HandlerFunc(appStyles))
	http.Handle("/", http.HandlerFunc(dashboard))
	http.Handle("/dashboard", http.HandlerFunc(dashboard))
	http.Handle("/websocket", http.HandlerFunc(websocketPage))
	http.Handle(voiceHub.Path(), voiceHub)
	if codexService != nil {
		http.Handle("/codex", http.HandlerFunc(codexPage))
		http.Handle(codexService.RegisterPath(), http.HandlerFunc(codexService.HandleRegister))
		http.Handle(codexService.EventPath(), http.HandlerFunc(codexService.HandleEvent))
		http.Handle("/v1/codex/sessions", http.HandlerFunc(codexService.HandleSessions))
		http.Handle("/v1/codex/sessions/", codexService)
	}
	if otaService != nil {
		http.Handle("/ota", http.HandlerFunc(otaPage))
		http.Handle("/v1/ota/check", http.HandlerFunc(otaService.HandleCheck))
		http.Handle("/v1/ota/firmware/", http.HandlerFunc(otaService.HandleFirmware))
		http.Handle("/v1/ota/releases", http.HandlerFunc(otaService.HandleReleases))
		http.Handle("/v1/ota/releases/", http.HandlerFunc(otaService.HandleRelease))
	}
	if webService != nil {
		http.Handle("/webapp", http.HandlerFunc(webAppPage))
		http.Handle("/v1/web/releases", http.HandlerFunc(webService.HandleReleases))
		http.Handle("/v1/web/releases/", http.HandlerFunc(webService.HandleRelease))
		http.Handle("/v1/web/current", http.HandlerFunc(webService.HandleCurrent))
		http.Handle("/ipad-show", http.HandlerFunc(webService.HandleApp))
		http.Handle("/ipad-show/", http.HandlerFunc(webService.HandleApp))
	}
	if ttsService != nil {
		http.Handle("/v1/devices", http.HandlerFunc(ttsService.HandleDevices))
		http.Handle("/v1/devices/", ttsService)
	}
	err = http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Printf("Listen Port Fail: %s", err)
	}
}

func appStyles(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/app.css" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	_, _ = w.Write([]byte(appCSS))
}

func websocketPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/websocket" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(websocketHTML))
}

func codexPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/codex" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(codexHTML))
}

func otaPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ota" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(otaHTML))
}

func webAppPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/webapp" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(webAppHTML))
}

func dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/dashboard" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(dashboardHTML))
}

func metrics(w http.ResponseWriter, r *http.Request) {
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sort.Slice(mfs, func(i, j int) bool {
		return mfs[i].GetName() < mfs[j].GetName()
	})
	for _, mf := range mfs {
		sort.Slice(mf.Metric, func(i, j int) bool {
			return metricSortKey(mf.Metric[i]) < metricSortKey(mf.Metric[j])
		})
	}
	w.Header().Set("Content-Type", string(expfmt.NewFormat(expfmt.TypeTextPlain)))
	for _, mf := range mfs {
		if _, err := expfmt.MetricFamilyToText(w, mf); err != nil {
			log.Printf("write metrics error: %s", err)
			return
		}
	}
}

func static(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "application/json")
	msg, err := json.Marshal(Result{Code: 200, Msg: "成功", Data: snapshotResultData()})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(msg)
}

func callEndpoint() {
	defer func() {
		if err := recover(); err != nil {
			log.Printf("callEndpoint error: %s", err)
		}
	}()

	mis := config.Mis

	go func() {
		for {
			for _, item := range mis {
				if !item.Asyn {
					callMiioctlItem(item)
				}
			}
			time.Sleep(time.Duration(daily) * time.Second)
		}
	}()

	for _, item := range mis {
		if item.Asyn {
			go func(mi Mi) {
				log.Printf("starting asynchronous poller for %s", mi.Name)
				for {
					callMiioctlItem(mi)
					time.Sleep(time.Duration(daily) * time.Second)
				}
			}(item)
		}
	}
}
func callTraffic(TrafficAddress string) {
	defer func() {
		if err := recover(); err != nil {
			log.Printf("callTraffic error: %s", err)
		}
	}()
	if TrafficAddress == "" {
		return
	}
	httpClient := &http.Client{Timeout: 1 * time.Second}
	resp, err := httpClient.Get(TrafficAddress)
	if err != nil {
		resultMu.Lock()
		setZero(resultData.Traffic)
		resultMu.Unlock()
		log.Printf("callTraffic call error: %s", err)
		return
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	data := string(body)
	if resp.StatusCode != 200 {
		resultMu.Lock()
		setZero(resultData.Traffic)
		resultMu.Unlock()
		log.Printf("callTraffic result error: %s", data)
		return
	}
	var traffic map[string]interface{}
	if err := json.Unmarshal(body, &traffic); err != nil {
		resultMu.Lock()
		setZero(resultData.Traffic)
		resultMu.Unlock()
		log.Printf("callTraffic unmarshal error: %s", err)
		return
	}
	resultMu.Lock()
	resultData.Traffic = traffic
	resultMu.Unlock()
}
func callMiioctlItem(item Mi) {
	defer func() {
		if err := recover(); err != nil {
			log.Printf("callMiioctlItem Miioctl: %s, error: %s", item.Name, err)
		}
	}()
	if item.Drive == "cuco" {
		// power
		cmd := exec.Command("miiocli", "genericmiot", "--ip", item.Ip, "--token", item.Token, "get_property_by", "11", "2")
		execSetValue(cmd, item, true)
		// temperature
		cmd = exec.Command("miiocli", "genericmiot", "--ip", item.Ip, "--token", item.Token, "get_property_by", "12", "2")
		execSetValue(cmd, item, false)
	} else if item.Drive == "iot" {
		// power
		cmd := exec.Command("miiocli", "genericmiot", "--ip", item.Ip, "--token", item.Token, "get_property_by", "3", "2")
		execSetValue(cmd, item, true)
	} else if item.Drive == "lumi.acpartner.mcn02" {
		// power
		cmd := exec.Command("miiocli", "airconditioningcompanionmcn02", "--ip", item.Ip, "--token", item.Token, "--model", item.Drive, "raw_command", "get_prop", "[\"load_power\"]")
		execSetValue(cmd, item, true)
	}
}

func execSetValue(cmd *exec.Cmd, item Mi, isPower bool) {
	// [{'did': '11-2', 'siid': 11, 'piid': 2, 'code': 0, 'value': 508}]
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	outStr, errStr := string(stdout.Bytes()), string(stderr.Bytes())
	if err != nil {
		panic(err)
	}
	if len(errStr) != 0 {
		log.Printf("Miioctl-1: %s, errStr: %s", item.Name, strings.Split(errStr, "\n")[0])
		return
	}
	if len(outStr) == 0 {
		return
	}
	list := strings.Split(outStr, "\n")
	outStr = list[1]
	outStr = strings.ReplaceAll(outStr, "'", "\"")
	var miioList []Miio
	var valueFloat float64
	if err = json.Unmarshal([]byte(outStr), &miioList); err == nil && len(miioList) > 0 {
		valueFloat = miioList[0].Value
	} else {
		var simpleArray []float64
		if err = json.Unmarshal([]byte(outStr), &simpleArray); err == nil && len(simpleArray) > 0 {
			valueFloat = simpleArray[0]
		} else {
			log.Printf("Miioctl-2: %s, outStr: %s, errStr: %s", item.Name, outStr, err)
			return
		}
	}
	log.Printf("Miioctl: %s, valueFloat: %f", item.Name, valueFloat)
	if isPower {
		miPlugPower.With(prometheus.Labels{"name": item.Name, "alias": itemAlias(item)}).Set(valueFloat)
		if len(item.HostName) > 0 {
			esxiTemperature.With(prometheus.Labels{"host_name": item.HostName, "alias": itemAlias(item)}).Set(valueFloat)
		}
		resultMu.Lock()
		resultData.Powers[item.Name] = valueFloat
		resultMu.Unlock()
	} else {
		miPlugTemperature.With(prometheus.Labels{"name": item.Name, "alias": itemAlias(item)}).Set(valueFloat)
		resultMu.Lock()
		resultData.Temperatures[item.Name] = valueFloat
		resultMu.Unlock()
	}
}

func snapshotResultData() ResultData {
	resultMu.RLock()
	defer resultMu.RUnlock()

	data := ResultData{
		Powers:       make(map[string]float64, len(resultData.Powers)),
		Temperatures: make(map[string]float64, len(resultData.Temperatures)),
		Traffic:      copyMap(resultData.Traffic),
		Devices:      make([]DeviceData, 0, len(config.Mis)),
	}
	if kiwiService != nil {
		kiwi := kiwiService.Snapshot()
		data.Kiwi = &kiwi
	}
	for key, value := range resultData.Powers {
		data.Powers[key] = value
	}
	for key, value := range resultData.Temperatures {
		data.Temperatures[key] = value
	}
	for _, item := range sortedMis() {
		device := DeviceData{
			Name:  item.Name,
			Alias: itemAlias(item),
			Sort:  item.Sort,
		}
		if value, ok := resultData.Powers[item.Name]; ok {
			valueCopy := value
			device.Power = &valueCopy
		}
		if value, ok := resultData.Temperatures[item.Name]; ok {
			valueCopy := value
			device.Temperature = &valueCopy
		}
		data.Devices = append(data.Devices, device)
	}
	return data
}

func sortedMis() []Mi {
	mis := make([]Mi, len(config.Mis))
	copy(mis, config.Mis)
	sort.SliceStable(mis, func(i, j int) bool {
		if mis[i].Sort != mis[j].Sort {
			return mis[i].Sort < mis[j].Sort
		}
		leftAlias := itemAlias(mis[i])
		rightAlias := itemAlias(mis[j])
		if leftAlias != rightAlias {
			return leftAlias < rightAlias
		}
		return mis[i].Name < mis[j].Name
	})
	return mis
}

func itemAlias(item Mi) string {
	if item.Alias != "" {
		return item.Alias
	}
	return item.Name
}

func metricSortKey(metric *dto.Metric) string {
	var parts []string
	for _, pair := range metric.Label {
		parts = append(parts, pair.GetName()+"="+pair.GetValue())
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func copyMap(data map[string]interface{}) map[string]interface{} {
	if data == nil {
		return nil
	}
	result := make(map[string]interface{}, len(data))
	for key, value := range data {
		result[key] = copyValue(value)
	}
	return result
}

func copyValue(value interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		return copyMap(v)
	case []interface{}:
		result := make([]interface{}, len(v))
		for i, item := range v {
			result[i] = copyValue(item)
		}
		return result
	default:
		return v
	}
}

// power-consumption:electric-power
// on-off-count:temperature
func parseData(data string) (power, temperature string) {
	pattern := `(?s)(power-consumption:electric-power|on-off-count:temperature)\D+(\d+)\s+None`
	re := regexp.MustCompile(pattern)
	matches := re.FindAllStringSubmatch(data, -1)
	for _, match := range matches {
		if match[1] == "power-consumption:electric-power" {
			power = match[2]
		}
		if match[1] == "on-off-count:temperature" {
			temperature = match[2]
		}
	}
	return power, temperature
}

func setZero(data interface{}) interface{} {
	switch v := data.(type) {
	case map[string]interface{}:
		for key, value := range v {
			v[key] = setZero(value)
		}
	case []interface{}:
		for i, value := range v {
			v[i] = setZero(value)
		}
	case int:
		return 0
	case float64:
		return 0
	case string:
		return "0"
	case bool:
		return false
	default:
		return v
	}
	return data
}
