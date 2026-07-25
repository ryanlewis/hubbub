package httpapi

import "encoding/json"

// A partial OpenAPI model: only the fields the docs page renders.
//
// Deliberately not a general-purpose OpenAPI parser. The one document it ever
// decodes is the one checked in next to it, so a field appearing here means the
// page draws it, and anything the spec grows that isn't here is simply not
// rendered rather than silently mis-rendered. `openapi_test.go` is what keeps
// the spec honest about the handler; this stays honest about the spec by
// covering so little of it.

type oaSpec struct {
	Info struct {
		Title       string `json:"title"`
		Version     string `json:"version"`
		Summary     string `json:"summary"`
		Description string `json:"description"`
	} `json:"info"`
	Servers []struct {
		URL string `json:"url"`
	} `json:"servers"`
	Paths      map[string]map[string]oaOperation `json:"paths"`
	Components struct {
		Schemas map[string]*oaSchema `json:"schemas"`
	} `json:"components"`
}

func (s *oaSpec) schema(name string) *oaSchema {
	if s == nil {
		return nil
	}
	return s.Components.Schemas[name]
}

type oaOperation struct {
	OperationID string `json:"operationId"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
	// A pointer so "absent" and "present but empty" stay distinguishable:
	// absent inherits the document's security requirement, `[]` opts out of it.
	// Collapsing the two would put a "bearer key required" badge on /health.
	Security    *[]json.RawMessage `json:"security"`
	RequestBody *struct {
		Required bool               `json:"required"`
		Content  map[string]oaMedia `json:"content"`
	} `json:"requestBody"`
	Responses map[string]struct {
		Description string             `json:"description"`
		Content     map[string]oaMedia `json:"content"`
	} `json:"responses"`
}

type oaMedia struct {
	Schema   *oaSchema       `json:"schema"`
	Example  json.RawMessage `json:"example"`
	Examples map[string]struct {
		Summary string          `json:"summary"`
		Value   json.RawMessage `json:"value"`
	} `json:"examples"`
}

type oaSchema struct {
	Ref         string               `json:"$ref"`
	Type        string               `json:"type"`
	Description string               `json:"description"`
	Enum        []any                `json:"enum"`
	Default     any                  `json:"default"`
	Const       any                  `json:"const"`
	Items       *oaSchema            `json:"items"`
	Properties  map[string]*oaSchema `json:"properties"`
	Required    []string             `json:"required"`
	// Pointers so a zero-valued limit is distinguishable from an absent one —
	// "≤ 0 bytes" on every unconstrained field would be worse than no column.
	MinLength *int `json:"minLength"`
	MaxLength *int `json:"maxLength"`
	MinItems  *int `json:"minItems"`
	MaxItems  *int `json:"maxItems"`
}
