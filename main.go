package main

import (
	"bytes"
	"flag"
	"log"
	"os/exec"
	"regexp"
)

func main() {
	var ip, token string
	flag.StringVar(&ip, "ip", "", "drive ip")
	flag.StringVar(&token, "token", "", "drive token")
	flag.Parse()
	if len(ip) == 0 || len(token) == 0 {
		log.Printf("param is null")
		return
	}

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
	log.Printf("%s\n", outStr)
	if len(errStr) != 0 {
		log.Printf("errorStr")
		log.Printf("%s\n", errStr)
	}
	if len(outStr) == 0 {
		return
	}
	power, temperature := parseData(outStr)
	log.Printf("功耗：%s W, 温度：%s C", power, temperature)
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
