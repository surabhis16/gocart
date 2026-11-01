package httpapi

import (
	"encoding/json"
	"net/http"
)

// Errors are RFC 9457 problem details (application/problem+json). The type is
// a URN rather than a URL: it identifies the kind of problem without pretending
// that a documentation page exists at that address.
const problemTypePrefix = "urn:gocart:problem:"

// Problem is an RFC 9457 problem document with optional extension members.
type Problem struct {
	Type   string
	Title  string
	Status int
	Detail string
	Extra  map[string]any
}

// MarshalJSON flattens Extra into the document. Standard members win over
// extensions of the same name.
func (p Problem) MarshalJSON() ([]byte, error) {
	m := map[string]any{
		"type":   problemTypePrefix + p.Type,
		"title":  p.Title,
		"status": p.Status,
	}
	if p.Detail != "" {
		m["detail"] = p.Detail
	}
	for k, v := range p.Extra {
		if _, taken := m[k]; !taken {
			m[k] = v
		}
	}
	return json.Marshal(m)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	if id := requestIDFrom(r.Context()); id != "" {
		if p.Extra == nil {
			p.Extra = map[string]any{}
		}
		p.Extra["request_id"] = id
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}
