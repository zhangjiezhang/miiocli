package main

import (
	"bytes"
	"flag"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"log"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
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
	name  = ""
	ip    = ""
	token = ""
)

func init() {
	prometheus.MustRegister(miPlugPower)
	prometheus.MustRegister(miPlugTemperature)
}

func main() {
	flag.StringVar(&name, "name", "", "drive name")
	flag.StringVar(&ip, "ip", "", "drive ip")
	flag.StringVar(&token, "token", "", "drive token")
	flag.Parse()
	if len(name) == 0 || len(ip) == 0 || len(token) == 0 {
		log.Printf("param is null")
		return
	}
	go func() {
		for {
			callMiioctl()
		}
	}()
	http.Handle("/metrics", promhttp.Handler())
	http.ListenAndServe(":8080", nil)
}

func callMiioctl() {
	cmd := exec.Command("miiocli", "genericmiot", "--ip", ip, "--token", token, "status")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	outStr, errStr := string(stdout.Bytes()), string(stderr.Bytes())
	if err != nil {
		log.Fatalf("cmd.Run() failed with %s\n", err)
		return
	}
	// log.Printf("%s\n", outStr)
	if len(errStr) != 0 {
		// log.Printf("errorStr")
		log.Printf("%s", errStr)
	}
	if len(outStr) == 0 {
		return
	}
	power, temperature := parseData(outStr)
	log.Printf("功耗：%s W, 温度：%s C", power, temperature)
	if len(power) != 0 {
		powerFloat, err := strconv.ParseFloat(power, 64)
		if err != nil {
			log.Printf("Error: %s", err)
		} else {
			miPlugPower.WithLabelValues("name", name).Set(powerFloat)
		}
	}
	if len(temperature) != 0 {
		temperatureFloat, err := strconv.ParseFloat(temperature, 64)
		if err != nil {
			log.Printf("Error: %s", err)
		} else {
			miPlugTemperature.WithLabelValues("name", name).Set(temperatureFloat)
		}
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
