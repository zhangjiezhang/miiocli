package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var (
	miPlugPower = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "mi_plug_power",
			Help: "小米智能插座功耗",
		},
		[]string{"name"},
	)
	miPlugTemperature = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "mi_plug_temperature",
			Help: "小米智能插座温度",
		},
		[]string{"name"},
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
		[]string{"host_name"},
	)
	filePath   = ""
	daily      float64
	config     Config
	resultData ResultData
)

type Config struct {
	TrafficAddress string `yaml:"trafficAddress"`
	Mis            []Mi   `yaml:"mis"`
}
type Mi struct {
	Name     string `yaml:"name"`
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
}
type Result struct {
	Code int        `json:"code"`
	Msg  string     `json:"msg"`
	Data ResultData `json:"data"`
}

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

	callEndpoint()
	go func() {
		for {
			callTraffic(config.TrafficAddress)
			time.Sleep(time.Duration(1) * time.Second)
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	http.Handle("/static", http.HandlerFunc(static))
	err = http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Printf("Listen Port Fail: %s", err)
	}
}

func static(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "application/json")
	msg, _ := json.Marshal(Result{Code: 200, Msg: "成功", Data: resultData})
	w.Write(msg)
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
			go func() {
				for {
					callMiioctlItem(item)
					time.Sleep(time.Duration(daily) * time.Second)
				}
			}()
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
		setZero(resultData.Traffic)
		log.Printf("callTraffic call error: %s", err)
		return
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	data := string(body)
	if resp.StatusCode != 200 {
		setZero(resultData.Traffic)
		log.Printf("callTraffic result error: %s", data)
		return
	}
	var traffic map[string]interface{}
	if err := json.Unmarshal(body, &traffic); err != nil {
		setZero(resultData.Traffic)
		log.Printf("callTraffic unmarshal error: %s", err)
		return
	}
	resultData.Traffic = traffic
}
func callMiioctlItem(item Mi) {
	defer func() {
		if err := recover(); err != nil {
			log.Printf("callMiioctlItem error: %s", err)
		}
	}()
	log.Printf("Miioctl: %s", item.Name)
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
		log.Printf("%s", errStr)
	}
	if len(outStr) == 0 {
		return
	}
	list := strings.Split(outStr, "\n")
	outStr = list[1]
	outStr = strings.ReplaceAll(outStr, "'", "\"")
	var miioList []Miio
	err = json.Unmarshal([]byte(outStr), &miioList)
	if err != nil {
		log.Printf("%s", errStr)
		return
	}
	valueFloat := miioList[0].Value
	log.Printf("valueFloat: %f", valueFloat)
	if err != nil {
		log.Printf("Error: %s", err)
	}
	if isPower {
		miPlugPower.With(prometheus.Labels{"name": item.Name}).Set(valueFloat)
		if len(item.HostName) > 0 {
			esxiTemperature.With(prometheus.Labels{"host_name": item.HostName}).Set(valueFloat)
		}
		resultData.Powers[item.Name] = valueFloat
	} else {
		miPlugTemperature.With(prometheus.Labels{"name": item.Name}).Set(valueFloat)
		resultData.Temperatures[item.Name] = valueFloat
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
