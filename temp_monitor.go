package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	// "github.com/fatih/color"
	"github.com/fatih/color"
	"github.com/ssimunic/gosensors"
)

type TemperatureMonitor struct {
	sensors      *gosensors.Sensors
	criticalTemp int // When to trigger cooldown (e.g. 85°C)
	safeTemp     int // When to resume (e.g. 65°C)
	useFallback  bool
}

var (
	tempWarn   = color.New(color.FgYellow).SprintFunc()
	tempDanger = color.New(color.FgRed).SprintFunc()
	tempNormal = color.New(color.FgGreen).SprintFunc()
)

func NewTemperatureMonitor(critical, safe int, isNotLocal bool) *TemperatureMonitor {
	color.Yellow("ℹ️  Temperature monitoring initialized")
	tm := &TemperatureMonitor{
		criticalTemp: critical,
		safeTemp:     safe,
	}

	// Graceful fallback if lm_sensors not available
	sensors, err := gosensors.NewFromSystem()
	if err != nil {
		color.Yellow("⚠️ lm_sensors not available - using time-based cooldown fallback")
		color.Blue("To enable sensor based monitoring please install lm_sensors to get this functionality running")
		tm.useFallback = true
	}
	tm.sensors = sensors
	// if _, err := tm.sensors.GetChips(); err != nil {
	// 	color.Yellow("⚠️ lm_sensors not available - using time-based cooldown fallback")
	// 	tm.useFallback = true
	// }
	if isNotLocal {
		color.Yellow("⚠️ Not using local GPU/CPU - using time-based cooldown fallback")
		tm.useFallback = true
	}
	return tm
}

func (tm *TemperatureMonitor) getTemperature() (int, string, error) {
	if tm.useFallback {
		return 0, "fallback", fmt.Errorf("lm_sensors not available")
	}

	chips := tm.sensors.Chips

	// Check GPU first
	for chip := range chips {
		for key, value := range tm.sensors.Chips[chip] {
			if key == "GPU" {
				// Remove the "°C" suffix
				temperatureString := strings.ReplaceAll(value, "°C", "")
				// Parse the string to a float64
				temperatureFloat, err := strconv.ParseFloat(temperatureString, 64)
				if err != nil {
					fmt.Println("Error parsing float for GPU:", err)
				}
				// Convert the float to an integer
				temperatureInt := int(temperatureFloat)
				if temperatureInt != 0 {
					return temperatureInt, "gpu", nil
				}

			}
		}
	}

	// Fallback to CPU if GPU not found
	for chip := range chips {
		for key, value := range tm.sensors.Chips[chip] {
			if key == "CPU" {
				// Remove the "°C" suffix
				temperatureString := strings.ReplaceAll(value, "°C", "")
				// Parse the string to a float64
				temperatureFloat, err := strconv.ParseFloat(temperatureString, 64)
				if err != nil {
					fmt.Println("Error parsing float for GPU:", err)
				}
				// Convert the float to an integer
				temperatureInt := int(temperatureFloat)
				if temperatureInt != 0 {
					return temperatureInt, "gpu", nil
				}
			}
		}
	}

	return 0, "unknown", fmt.Errorf("no temperature sensors found")
}

func (tm *TemperatureMonitor) CoolDown() error {
	start := time.Now()

	for {
		temp, source, err := tm.getTemperature()
		if err != nil {
			color.Yellow("⚠️ Temperature monitoring unavailable - defaulting to 60s cooldown")
			time.Sleep(60 * time.Second)
			return nil
		}

		// Handle zero readings
		if temp == 0 {
			color.Blue("❄️  Zero temperature reading - assuming CPU-only mode")
			source = "cpu"
		}

		tempMsg := fmt.Sprintf("%.1f°C", temp)
		if temp >= tm.criticalTemp {
			tempMsg = tempDanger(tempMsg)
		} else if temp >= tm.safeTemp {
			tempMsg = tempWarn(tempMsg)
		} else {
			tempMsg = tempNormal(tempMsg)
		}

		fmt.Printf("\r🌡 [%s] Current %s Temp: %s (Cooling since %v)",
			time.Now().Format("15:04:05"),
			strings.ToUpper(source),
			tempMsg,
			time.Since(start).Round(time.Second))

		if temp < tm.safeTemp {
			fmt.Println("\n✅ Temperature normalized")
			return nil
		}

		// Dynamic cooldown calculation
		waitSec := 2
		if temp > tm.criticalTemp {
			waitSec = 5 + int(temp-tm.safeTemp)
		}
		time.Sleep(time.Duration(waitSec) * time.Second)
	}
}

// func main() {
// 	tempMonitor := NewTemperatureMonitor(85, 65)
// 	temp, source, _ := tempMonitor.getTemperature()
// 	fmt.Println(source)
// 	fmt.Println(temp)
// 	fmt.Sprintf("%f", temp)
// }
