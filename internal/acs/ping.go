package acs

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/prawirdani/opticore/internal/cwmp"
)

// PingArgs is the args of a ping task.
type PingArgs struct {
	Host      string `json:"host"`
	Count     int    `json:"count"`
	TimeoutMS int    `json:"timeout_ms"`
	Size      int    `json:"size"`
	Interface string `json:"interface"`
}

const (
	pingDefaultCount     = 4
	pingDefaultTimeoutMS = 1000
	pingDefaultSize      = 64

	pingMaxCount     = 100
	pingMaxTimeoutMS = 60000
	pingMinSize      = 8
	pingMaxSize      = 65535
)

// parsePingArgs validates and fills the ping defaults.
func parsePingArgs(raw json.RawMessage) (PingArgs, error) {
	var a PingArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return a, errors.New(`args must be {"host": "...", "count": 4, "timeout_ms": 1000, "size": 64}`)
		}
	}
	if a.Host == "" {
		return a, errors.New("host is required")
	}
	if strings.ContainsAny(a.Host, " \t\r\n") {
		return a, errors.New("host must not contain whitespace")
	}
	if a.Count == 0 {
		a.Count = pingDefaultCount
	}
	if a.TimeoutMS == 0 {
		a.TimeoutMS = pingDefaultTimeoutMS
	}
	if a.Size == 0 {
		a.Size = pingDefaultSize
	}
	if a.Count < 1 || a.Count > pingMaxCount {
		return a, fmt.Errorf("count must be between 1 and %d", pingMaxCount)
	}
	if a.TimeoutMS < 1 || a.TimeoutMS > pingMaxTimeoutMS {
		return a, fmt.Errorf("timeout_ms must be between 1 and %d", pingMaxTimeoutMS)
	}
	if a.Size < pingMinSize || a.Size > pingMaxSize {
		return a, fmt.Errorf("size must be between %d and %d", pingMinSize, pingMaxSize)
	}
	return a, nil
}

// pingObject maps the device root reported in the Inform to the IPPing
// diagnostics object.
func pingObject(root string) (string, error) {
	switch root {
	case "InternetGatewayDevice.":
		return "InternetGatewayDevice.IPPingDiagnostics.", nil
	case "Device.":
		return "Device.IP.Diagnostics.IPPing.", nil
	}
	return "", fmt.Errorf("device root %q is unknown; cannot locate IPPingDiagnostics", root)
}

func pingResultNames(obj string) []string {
	leaves := []string{"DiagnosticsState", "SuccessCount", "FailureCount",
		"AverageResponseTime", "MinimumResponseTime", "MaximumResponseTime"}
	out := make([]string, len(leaves))
	for i, leaf := range leaves {
		out[i] = obj + leaf
	}
	return out
}

// pingSet starts the diagnostics: a SetParameterValues with
// DiagnosticsState=Requested, then the device runs the ping on its own.
func pingSet(obj string, a PingArgs) []cwmp.ParamValue {
	vals := []cwmp.ParamValue{
		{Name: obj + "DiagnosticsState", Value: "Requested", Type: "xsd:string"},
		{Name: obj + "Host", Value: a.Host, Type: "xsd:string"},
		{Name: obj + "NumberOfRepetitions", Value: strconv.Itoa(a.Count), Type: "xsd:unsignedInt"},
		{Name: obj + "Timeout", Value: strconv.Itoa(a.TimeoutMS), Type: "xsd:unsignedInt"},
		{Name: obj + "DataBlockSize", Value: strconv.Itoa(a.Size), Type: "xsd:unsignedInt"},
	}
	if a.Interface != "" {
		vals = append(vals, cwmp.ParamValue{Name: obj + "Interface", Value: a.Interface, Type: "xsd:string"})
	}
	return vals
}

// pingResult reads the result GPV into the task result shape.
func pingResult(obj string, params []cwmp.ParamValue) (string, map[string]any) {
	byLeaf := make(map[string]string, len(params))
	for _, p := range params {
		name := p.Name
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
		byLeaf[name] = p.Value
	}
	state := byLeaf["DiagnosticsState"]
	res := map[string]any{
		"state":   state,
		"success": atoiOrZero(byLeaf["SuccessCount"]),
		"failure": atoiOrZero(byLeaf["FailureCount"]),
		"avg_ms":  atoiOrZero(byLeaf["AverageResponseTime"]),
		"min_ms":  atoiOrZero(byLeaf["MinimumResponseTime"]),
		"max_ms":  atoiOrZero(byLeaf["MaximumResponseTime"]),
	}
	return state, res
}

func atoiOrZero(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
