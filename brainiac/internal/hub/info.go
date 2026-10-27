package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Details is what the Hub says about one model: enough for the scout to
// decide whether it is worth a person's look, never enough to use it.
type Details struct {
	ID           string    `json:"id"`
	CreatedAt    time.Time `json:"created_at"`
	LastModified time.Time `json:"last_modified"`
	Gated        string    `json:"gated,omitempty"` // "", "auto" or "manual"
	PipelineTag  string    `json:"pipeline_tag,omitempty"`
	Tags         []string  `json:"tags,omitempty"`
	Downloads    int       `json:"downloads"`
	Likes        int       `json:"likes"`
	License      string    `json:"license,omitempty"`
	BaseModels   []string  `json:"base_models,omitempty"`
	// GGUF is the Hub's reading of the repo's GGUF headers, when it has some.
	Architecture  string  `json:"architecture,omitempty"`
	ParamsB       float64 `json:"params_b,omitempty"`
	ContextLength int     `json:"context_length,omitempty"`
	// Claims are benchmark results the model card states (model-index):
	// the vendor's numbers, kept to show a person, never used as fact.
	Claims []Claim `json:"claims,omitempty"`
}

// Claim is one benchmark result from a model card.
type Claim struct {
	Dataset string  `json:"dataset"`
	Metric  string  `json:"metric"`
	Value   float64 `json:"value"`
}

// Details reads /api/models/{repo}.
func (c *Client) Details(ctx context.Context, repo string) (Details, error) {
	p, err := repoPath(repo)
	if err != nil {
		return Details{}, err
	}
	var raw struct {
		ID           string    `json:"id"`
		CreatedAt    time.Time `json:"createdAt"`
		LastModified time.Time `json:"lastModified"`
		Gated        any       `json:"gated"`
		PipelineTag  string    `json:"pipeline_tag"`
		Tags         []string  `json:"tags"`
		Downloads    int       `json:"downloads"`
		Likes        int       `json:"likes"`
		CardData     struct {
			License     any    `json:"license"`
			LicenseName string `json:"license_name"`
			BaseModel   any    `json:"base_model"`
			ModelIndex  []struct {
				Results []struct {
					Dataset struct {
						Name string `json:"name"`
						Type string `json:"type"`
					} `json:"dataset"`
					Metrics []struct {
						Type  string `json:"type"`
						Name  string `json:"name"`
						Value any    `json:"value"`
					} `json:"metrics"`
				} `json:"results"`
			} `json:"model-index"`
		} `json:"cardData"`
		GGUF *struct {
			Total         float64 `json:"total"`
			Architecture  string  `json:"architecture"`
			ContextLength int     `json:"context_length"`
		} `json:"gguf"`
		Safetensors *struct {
			Total float64 `json:"total"`
		} `json:"safetensors"`
	}
	if _, err := c.getJSON(ctx, c.endpoint()+"/api/models/"+p, &raw); err != nil {
		return Details{}, err
	}
	d := Details{ID: raw.ID, CreatedAt: raw.CreatedAt, LastModified: raw.LastModified, PipelineTag: raw.PipelineTag,
		Tags: raw.Tags, Downloads: raw.Downloads, Likes: raw.Likes}
	switch g := raw.Gated.(type) {
	case string:
		d.Gated = g
	case bool:
		if g {
			d.Gated = "manual"
		}
	}
	d.License = firstString(raw.CardData.License)
	if (d.License == "other" || d.License == "") && raw.CardData.LicenseName != "" {
		d.License = raw.CardData.LicenseName
	}
	if d.License == "" {
		for _, t := range raw.Tags {
			if strings.HasPrefix(t, "license:") {
				d.License = strings.TrimPrefix(t, "license:")
			}
		}
	}
	d.BaseModels = strings_(raw.CardData.BaseModel)
	if raw.GGUF != nil {
		d.Architecture, d.ContextLength = raw.GGUF.Architecture, raw.GGUF.ContextLength
		d.ParamsB = raw.GGUF.Total / 1e9
	}
	if d.ParamsB == 0 && raw.Safetensors != nil {
		d.ParamsB = raw.Safetensors.Total / 1e9
	}
	for _, mi := range raw.CardData.ModelIndex {
		for _, r := range mi.Results {
			for _, m := range r.Metrics {
				v, ok := number(m.Value)
				if !ok {
					continue
				}
				name := r.Dataset.Name
				if name == "" {
					name = r.Dataset.Type
				}
				metric := m.Name
				if metric == "" {
					metric = m.Type
				}
				d.Claims = append(d.Claims, Claim{Dataset: name, Metric: metric, Value: v})
				if len(d.Claims) >= 12 {
					return d, nil
				}
			}
		}
	}
	return d, nil
}

func firstString(v any) string {
	if s := strings_(v); len(s) > 0 {
		return s[0]
	}
	return ""
}

// strings_ reads a card field that may be a string or a list of strings.
func strings_(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		var f float64
		_, err := fmt.Sscan(strings.TrimSuffix(x, "%"), &f)
		return f, err == nil
	}
	return 0, false
}
