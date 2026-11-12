package cwmp

import "strconv"

var (
	envelopePrefixes [5][]byte // 1.0, 1.1, 1.2, 1.3, 1.4
	envelopeMid      = []byte(`</cwmp:ID></soap-env:Header><soap-env:Body>`)
	envelopeSuffix   = []byte(`</soap-env:Body></soap-env:Envelope>`)

	envelopeHead = []byte(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<soap-env:Envelope xmlns:soap-enc="http://schemas.xmlsoap.org/soap/encoding/" xmlns:soap-env="http://schemas.xmlsoap.org/soap/envelope/" xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:cwmp="`)
	envelopeIDOpen = []byte(`"><soap-env:Header><cwmp:ID soap-env:mustUnderstand="1">`)

	cwmpNamespaces = [5]string{
		"urn:dslforum-org:cwmp-1-0",
		"urn:dslforum-org:cwmp-1-1",
		"urn:dslforum-org:cwmp-1-2",
		"urn:dslforum-org:cwmp-1-2", // 1.3 keeps the 1-2 namespace
		"urn:dslforum-org:cwmp-1-3",
	}
)

func init() {
	for i, ns := range cwmpNamespaces {
		b := make([]byte, 0, len(envelopeHead)+len(ns)+len(envelopeIDOpen))
		b = append(b, envelopeHead...)
		b = append(b, ns...)
		b = append(b, envelopeIDOpen...)
		envelopePrefixes[i] = b
	}
}

func versionIndex(ver string) int {
	switch ver {
	case "1.1":
		return 1
	case "1.2":
		return 2
	case "1.3":
		return 3
	case "1.4":
		return 4
	}
	return 0
}

func appendEnvelope(dst []byte, ver, id string) []byte {
	dst = append(dst, envelopePrefixes[versionIndex(ver)]...)
	dst = appendEscaped(dst, id, false)
	dst = append(dst, envelopeMid...)
	return dst
}

func AppendInformResponse(dst []byte, ver, id string) []byte {
	dst = appendEnvelope(dst, ver, id)
	dst = append(dst, `<cwmp:InformResponse><MaxEnvelopes>1</MaxEnvelopes></cwmp:InformResponse>`...)
	return append(dst, envelopeSuffix...)
}

func AppendGetRPCMethodsResponse(dst []byte, ver, id string) []byte {
	dst = appendEnvelope(dst, ver, id)
	dst = append(dst, `<cwmp:GetRPCMethodsResponse><MethodList soap-enc:arrayType="xsd:string[3]"><string>Inform</string><string>GetRPCMethods</string><string>TransferComplete</string></MethodList></cwmp:GetRPCMethodsResponse>`...)
	return append(dst, envelopeSuffix...)
}

func AppendTransferCompleteResponse(dst []byte, ver, id string) []byte {
	dst = appendEnvelope(dst, ver, id)
	dst = append(dst, `<cwmp:TransferCompleteResponse/>`...)
	return append(dst, envelopeSuffix...)
}

func AppendGetParameterValues(dst []byte, ver, id string, names []string) []byte {
	dst = appendEnvelope(dst, ver, id)
	dst = append(dst, `<cwmp:GetParameterValues><ParameterNames soap-enc:arrayType="xsd:string[`...)
	dst = strconv.AppendInt(dst, int64(len(names)), 10)
	dst = append(dst, `]">`...)
	for _, n := range names {
		dst = append(dst, `<string>`...)
		dst = appendEscaped(dst, n, false)
		dst = append(dst, `</string>`...)
	}
	dst = append(dst, `</ParameterNames></cwmp:GetParameterValues>`...)
	return append(dst, envelopeSuffix...)
}

func AppendGetParameterNames(dst []byte, ver, id, path string, nextLevel bool) []byte {
	dst = appendEnvelope(dst, ver, id)
	dst = append(dst, `<cwmp:GetParameterNames><ParameterPath>`...)
	dst = appendEscaped(dst, path, false)
	dst = append(dst, `</ParameterPath><NextLevel>`...)
	dst = appendBool(dst, nextLevel)
	dst = append(dst, `</NextLevel></cwmp:GetParameterNames>`...)
	return append(dst, envelopeSuffix...)
}

func AppendSetParameterValues(dst []byte, ver, id string, params []ParamValue, parameterKey string) []byte {
	dst = appendEnvelope(dst, ver, id)
	dst = append(dst, `<cwmp:SetParameterValues><ParameterList soap-enc:arrayType="cwmp:ParameterValueStruct[`...)
	dst = strconv.AppendInt(dst, int64(len(params)), 10)
	dst = append(dst, `]">`...)
	for i := range params {
		p := &params[i]
		dst = append(dst, `<ParameterValueStruct><Name>`...)
		dst = appendEscaped(dst, p.Name, false)
		dst = append(dst, `</Name><Value xsi:type="`...)
		if p.Type == "" {
			dst = append(dst, `xsd:string`...)
		} else {
			dst = appendEscaped(dst, p.Type, true)
		}
		dst = append(dst, `">`...)
		dst = appendEscaped(dst, p.Value, false)
		dst = append(dst, `</Value></ParameterValueStruct>`...)
	}
	dst = append(dst, `</ParameterList><ParameterKey>`...)
	dst = appendEscaped(dst, parameterKey, false)
	dst = append(dst, `</ParameterKey></cwmp:SetParameterValues>`...)
	return append(dst, envelopeSuffix...)
}

func AppendAddObject(dst []byte, ver, id, objectName, parameterKey string) []byte {
	dst = appendEnvelope(dst, ver, id)
	dst = append(dst, `<cwmp:AddObject><ObjectName>`...)
	dst = appendEscaped(dst, objectName, false)
	dst = append(dst, `</ObjectName><ParameterKey>`...)
	dst = appendEscaped(dst, parameterKey, false)
	dst = append(dst, `</ParameterKey></cwmp:AddObject>`...)
	return append(dst, envelopeSuffix...)
}

func AppendDeleteObject(dst []byte, ver, id, objectName, parameterKey string) []byte {
	dst = appendEnvelope(dst, ver, id)
	dst = append(dst, `<cwmp:DeleteObject><ObjectName>`...)
	dst = appendEscaped(dst, objectName, false)
	dst = append(dst, `</ObjectName><ParameterKey>`...)
	dst = appendEscaped(dst, parameterKey, false)
	dst = append(dst, `</ParameterKey></cwmp:DeleteObject>`...)
	return append(dst, envelopeSuffix...)
}

func AppendReboot(dst []byte, ver, id, commandKey string) []byte {
	dst = appendEnvelope(dst, ver, id)
	dst = append(dst, `<cwmp:Reboot><CommandKey>`...)
	dst = appendEscaped(dst, commandKey, false)
	dst = append(dst, `</CommandKey></cwmp:Reboot>`...)
	return append(dst, envelopeSuffix...)
}

func AppendFactoryReset(dst []byte, ver, id string) []byte {
	dst = appendEnvelope(dst, ver, id)
	dst = append(dst, `<cwmp:FactoryReset/>`...)
	return append(dst, envelopeSuffix...)
}

func AppendDownload(dst []byte, ver, id, commandKey, fileType, url, username, password string, fileSize int64, targetFileName string, delaySeconds int, successURL, failureURL string) []byte {
	dst = appendEnvelope(dst, ver, id)
	dst = append(dst, `<cwmp:Download><CommandKey>`...)
	dst = appendEscaped(dst, commandKey, false)
	dst = append(dst, `</CommandKey><FileType>`...)
	dst = appendEscaped(dst, fileType, false)
	dst = append(dst, `</FileType><URL>`...)
	dst = appendEscaped(dst, url, false)
	dst = append(dst, `</URL><Username>`...)
	dst = appendEscaped(dst, username, false)
	dst = append(dst, `</Username><Password>`...)
	dst = appendEscaped(dst, password, false)
	dst = append(dst, `</Password><FileSize>`...)
	dst = strconv.AppendInt(dst, fileSize, 10)
	dst = append(dst, `</FileSize><TargetFileName>`...)
	dst = appendEscaped(dst, targetFileName, false)
	dst = append(dst, `</TargetFileName><DelaySeconds>`...)
	dst = strconv.AppendInt(dst, int64(delaySeconds), 10)
	dst = append(dst, `</DelaySeconds><SuccessURL>`...)
	dst = appendEscaped(dst, successURL, false)
	dst = append(dst, `</SuccessURL><FailureURL>`...)
	dst = appendEscaped(dst, failureURL, false)
	dst = append(dst, `</FailureURL></cwmp:Download>`...)
	return append(dst, envelopeSuffix...)
}

func appendBool(dst []byte, v bool) []byte {
	if v {
		return append(dst, '1')
	}
	return append(dst, '0')
}

// appendEscaped copies s escaping the XML metacharacters. Quotes are only
// escaped inside attribute values.
func appendEscaped(dst []byte, s string, attr bool) []byte {
	start := 0
	for i := 0; i < len(s); i++ {
		var esc string
		switch s[i] {
		case '&':
			esc = "&amp;"
		case '<':
			esc = "&lt;"
		case '>':
			esc = "&gt;"
		case '"':
			if !attr {
				continue
			}
			esc = "&quot;"
		case '\'':
			if !attr {
				continue
			}
			esc = "&apos;"
		default:
			continue
		}
		dst = append(dst, s[start:i]...)
		dst = append(dst, esc...)
		start = i + 1
	}
	return append(dst, s[start:]...)
}
