package cwmp

import "errors"

// ACSRequest is a decoded ACS-to-CPE RPC. Only Request, Response and Close are
// needed to serve a device; this decoder exists for the simulator and the
// conformance tests that play a CPE.
type ACSRequest struct {
	ID    string
	Kind  RequestKind
	Names []string
	Path  string
	Next  bool
	Vals  []ParamValue
	Key   string
}

var requestKinds = map[string]RequestKind{
	"GetParameterValues": ReqGetParameterValues,
	"GetParameterNames":  ReqGetParameterNames,
	"SetParameterValues": ReqSetParameterValues,
	"AddObject":          ReqAddObject,
	"DeleteObject":       ReqDeleteObject,
	"Reboot":             ReqReboot,
	"FactoryReset":       ReqFactoryReset,
	"Download":           ReqDownload,
}

// DecodeACSRequest parses an envelope written by Append* in write.go.
func DecodeACSRequest(b []byte) (*ACSRequest, error) {
	var d Doc
	if err := d.Parse(b); err != nil {
		return nil, err
	}
	env := int32(0)
	if len(d.Nodes) == 0 || string(d.Nodes[env].Local) != "Envelope" {
		return nil, errNoEnvel
	}
	body := d.Child(env, "Body")
	if body < 0 {
		return nil, errNoBody
	}
	method := d.Nodes[body].FirstChild
	if method < 0 {
		return nil, errNoMethod
	}
	r := &ACSRequest{}
	kind, ok := requestKinds[string(d.Nodes[method].Local)]
	if !ok {
		return nil, errors.New("cwmp: unknown ACS request " + string(d.Nodes[method].Local))
	}
	r.Kind = kind
	if header := d.Child(env, "Header"); header >= 0 {
		if id := d.Child(header, "ID"); id >= 0 {
			r.ID = string(d.Nodes[id].Text)
		}
	}
	switch kind {
	case ReqGetParameterValues, ReqGetParameterNames:
		name := "ParameterNames"
		if kind == ReqGetParameterNames {
			name = "ParameterName"
		}
		if list := d.Child(method, name); list >= 0 {
			if kind == ReqGetParameterValues {
				for c := d.Nodes[list].FirstChild; c >= 0; c = d.Nodes[c].NextSibling {
					r.Names = append(r.Names, string(d.Nodes[c].Text))
				}
			} else {
				r.Path = string(d.Nodes[list].Text)
			}
		}
		if kind == ReqGetParameterNames {
			if nl := d.Child(method, "NextLevel"); nl >= 0 {
				r.Next = string(d.Nodes[nl].Text) == "1" || string(d.Nodes[nl].Text) == "true"
			}
		}
	case ReqSetParameterValues:
		if list := d.Child(method, "ParameterList"); list >= 0 {
			r.Vals = d.decodeParamValues(list)
		}
		if k := d.Child(method, "ParameterKey"); k >= 0 {
			r.Key = string(d.Nodes[k].Text)
		}
	case ReqAddObject, ReqDeleteObject:
		if o := d.Child(method, "ObjectName"); o >= 0 {
			r.Path = string(d.Nodes[o].Text)
		}
		if k := d.Child(method, "ParameterKey"); k >= 0 {
			r.Key = string(d.Nodes[k].Text)
		}
	case ReqReboot, ReqDownload:
		if k := d.Child(method, "CommandKey"); k >= 0 {
			r.Key = string(d.Nodes[k].Text)
		}
	}
	return r, nil
}
