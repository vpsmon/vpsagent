package cloud

import (
	"math"
	"unicode/utf8"

	"github.com/vpsmon/vpsmonlib/metrics"
)

const maxTelemetryBytes = 256 << 10

func boundedText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func validPercent(value, maximum float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= maximum
}

func boundedProcesses(processes []metrics.TopProcess) []metrics.TopProcess {
	result := make([]metrics.TopProcess, 0, min(len(processes), 50))
	for _, process := range processes {
		if !validPercent(process.CPU, 10000) || !validPercent(process.Mem, 100) {
			continue
		}
		process.Name = boundedText(process.Name, 255)
		result = append(result, process)
		if len(result) == 50 {
			break
		}
	}
	return result
}

func boundedContainers(containers []metrics.DockerContainer) []metrics.DockerContainer {
	result := make([]metrics.DockerContainer, 0, min(len(containers), 100))
	for _, container := range containers {
		if !validPercent(container.CPU, 10000) || !validPercent(container.Mem, 100) || container.RestartCount < 0 {
			continue
		}
		container.ID = boundedText(container.ID, 128)
		container.Name = boundedText(container.Name, 255)
		container.Image = boundedText(container.Image, 1024)
		container.Status = boundedText(container.Status, 255)
		container.Health = boundedText(container.Health, 128)
		container.Uptime = boundedText(container.Uptime, 128)
		container.Ports = boundedText(container.Ports, 1024)
		result = append(result, container)
		if len(result) == 100 {
			break
		}
	}
	return result
}
