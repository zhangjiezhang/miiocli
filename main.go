package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v2"
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
	mi         []Mi
	resultData ResultData
)

type Mi struct {
	Name     string `yaml:"name"`
	Ip       string `yaml:"ip"`
	Token    string `yaml:"token"`
	Drive    string `yaml:"drive"`
	HostName string `yaml:"hostName"`
}

// [{'did': '11-2', 'siid': 11, 'piid': 2, 'code': 0, 'value': 508}]
type Miio struct {
	Did   string  `yaml:"did"`
	Siid  int     `yaml:"siid"`
	Piid  int     `yaml:"piid"`
	Code  int     `yaml:"code"`
	Value float64 `yaml:"value"`
}

type StaticData struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}
type ResultData struct {
	Powers       []StaticData `json:"powers"`
	Temperatures []StaticData `json:"temperatures"`
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
		log.Printf("param is null")
		return
	}
	file, err := os.ReadFile(filePath)
	if err != nil {
		log.Printf("读取配置文件失败 #%v", err)
		return
	}
	err = yaml.Unmarshal(file, &mi)
	if err != nil {
		log.Fatalf("解析失败: %v", err)
		return
	}
	//prometheus.MustRegister(miPlugPower)
	//prometheus.MustRegister(miPlugTemperature)
	go func() {
		for {
			callMiioctl()
			time.Sleep(time.Duration(daily) * time.Second)
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

func callMiioctl() {
	defer func() {
		if err := recover(); err != nil {
			log.Fatalf("callMiioctl error: %s", err)
		}
	}()
	for _, item := range mi {
		callMiioctlItem(item)
	}
}
func callMiioctlItem(item Mi) {
	var powers []StaticData
	var temperatures []StaticData
	defer func() {
		if err := recover(); err != nil {
			log.Fatalf("callMiioctlItem error: %s", err)
		}
	}()

	if item.Drive == "cuco" {
		// power
		cmd := exec.Command("miiocli", "genericmiot", "--ip", item.Ip, "--token", item.Token, "get_property_by", "11", "2")
		powers = execSetValue(cmd, item, true, powers)
		// temperature
		cmd = exec.Command("miiocli", "genericmiot", "--ip", item.Ip, "--token", item.Token, "get_property_by", "12", "2")
		temperatures = execSetValue(cmd, item, false, temperatures)
	} else if item.Drive == "iot" {
		// power
		cmd := exec.Command("miiocli", "genericmiot", "--ip", item.Ip, "--token", item.Token, "get_property_by", "3", "2")
		powers = execSetValue(cmd, item, true, powers)
	}
	resultData = ResultData{Powers: powers, Temperatures: temperatures}
}

func execSetValue(cmd *exec.Cmd, item Mi, isPower bool, statics []StaticData) []StaticData {
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
		return statics
	}
	list := strings.Split(outStr, "\n")
	outStr = list[1]
	outStr = strings.ReplaceAll(outStr, "'", "\"")
	var miioList []Miio
	err = json.Unmarshal([]byte(outStr), &miioList)
	if err != nil {
		log.Printf("%s", errStr)
		return statics
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
	} else {
		miPlugTemperature.With(prometheus.Labels{"name": item.Name}).Set(valueFloat)
	}
	staticData := StaticData{Name: item.Name, Value: valueFloat}
	return append(statics, staticData)
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
