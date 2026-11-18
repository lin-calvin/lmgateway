// Command genplanpresets regenerates internal/config/plan_presets.json from the
// OpenCode Go model catalog. Codex plans are static (window-share), and Zen is
// pay-as-you-go, not a subscription plan, so it is intentionally excluded.
//
// Run from the repository root:
//
//	go run ./cmd/genplanpresets
//	go run ./cmd/genplanpresets -check
//
// The generated file is committed; the build never fetches the network.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	defaultGoModelsURL  = "https://opencode.ai/inference/go/v1/models"
	opencodeGoPrice     = 10.0
	opencodeGoPlusPrice = 40.0
)

type catalog struct {
	Data []catalogModel `json:"data"`
}

type catalogModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	Cost *struct {
		Text *struct {
			Input struct {
				Price float64 `json:"price"`
				Cache struct {
					Read float64 `json:"read"`
				} `json:"cache"`
			} `json:"input"`
			Output struct {
				Price float64 `json:"price"`
			} `json:"output"`
		} `json:"text"`
	} `json:"cost"`
	Go *struct {
		MonthlyUsage map[string]float64 `json:"monthly_usage"`
	} `json:"go"`
}

type modelPrice struct {
	InputPerMtok       float64 `json:"input_per_mtok"`
	CachedInputPerMtok float64 `json:"cached_input_per_mtok"`
	OutputPerMtok      float64 `json:"output_per_mtok"`
}

type preset struct {
	Name            string                `json:"name"`
	Price           float64               `json:"price"`
	Currency        string                `json:"currency"`
	Period          string                `json:"period"`
	CostModel       string                `json:"cost_model"`
	Multipliers     map[string]float64    `json:"multipliers,omitempty"`
	Window          string                `json:"window,omitempty"`
	RateLimitFamily string                `json:"rate_limit_family"`
	Models          map[string]modelPrice `json:"models,omitempty"`
}

func main() {
	out := flag.String("out", filepath.Join("internal", "config", "plan_presets.json"), "output JSON path")
	goURL := flag.String("go-url", defaultGoModelsURL, "OpenCode Go models endpoint")
	check := flag.Bool("check", false, "exit non-zero if the checked-in file is stale")
	timeout := flag.Duration("timeout", 30*time.Second, "HTTP timeout")
	minModels := flag.Int("min-models", 5, "refuse to write a suspiciously small catalog")
	flag.Parse()

	catalog, err := fetchCatalog(*goURL, *timeout)
	if err != nil {
		fatal("fetch Go catalog: %v", err)
	}
	cat := catalogData(catalog)
	if len(cat.Prices) < *minModels {
		fatal("Go catalog returned only %d priced models (minimum %d); refusing to overwrite", len(cat.Prices), *minModels)
	}

	presets := map[string]preset{
		"opencode_go": {
			Name:            "OpenCode Go",
			Price:           opencodeGoPrice,
			Currency:        "USD",
			Period:          "month",
			CostModel:       "multiplier",
			Multipliers:     cat.GoMultipliers,
			RateLimitFamily: "opencode",
			Models:          cat.Prices,
		},
		"opencode_go_plus": {
			Name:            "OpenCode Go Plus",
			Price:           opencodeGoPlusPrice,
			Currency:        "USD",
			Period:          "month",
			CostModel:       "multiplier",
			Multipliers:     cat.PlusMultipliers,
			RateLimitFamily: "opencode",
			Models:          cat.Prices,
		},
		"chatgpt_plus": {
			Name:            "ChatGPT Plus (Codex)",
			Price:           20,
			Currency:        "USD",
			Period:          "month",
			CostModel:       "window_share",
			Window:          "secondary",
			RateLimitFamily: "codex",
		},
		"chatgpt_pro": {
			Name:            "ChatGPT Pro (Codex)",
			Price:           200,
			Currency:        "USD",
			Period:          "month",
			CostModel:       "window_share",
			Window:          "secondary",
			RateLimitFamily: "codex",
		},
	}

	data, err := json.MarshalIndent(presets, "", "  ")
	if err != nil {
		fatal("marshal presets: %v", err)
	}
	data = append(data, '\n')

	if *check {
		existing, err := os.ReadFile(*out)
		if err != nil {
			fatal("read %s: %v", *out, err)
		}
		if !bytes.Equal(existing, data) {
			fatal("%s is stale; run: go run ./cmd/genplanpresets", *out)
		}
		fmt.Printf("%s is up to date (%d Go models)\n", *out, len(cat.Prices))
		return
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fatal("mkdir: %v", err)
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fatal("write %s: %v", *out, err)
	}
	fmt.Printf("wrote %s (%d Go models)\n", *out, len(cat.Prices))
}

func fetchCatalog(url string, timeout time.Duration) (*catalog, error) {
	client := &http.Client{Timeout: timeout}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "lmgateway-plan-presets/1.0")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("GET %s: %s", url, resp.Status)
			resp.Body.Close()
			continue
		}
		var c catalog
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&c)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return &c, nil
	}
	return nil, lastErr
}

type catalogDataResult struct {
	Prices          map[string]modelPrice
	GoMultipliers   map[string]float64
	PlusMultipliers map[string]float64
}

func catalogData(c *catalog) catalogDataResult {
	out := catalogDataResult{
		Prices:          map[string]modelPrice{},
		GoMultipliers:   map[string]float64{"default": 1},
		PlusMultipliers: map[string]float64{"default": 1},
	}
	for _, m := range c.Data {
		if m.Type != "" && m.Type != "chat" {
			continue
		}
		if m.Cost == nil || m.Cost.Text == nil {
			continue
		}
		key := m.ID
		if key == "" {
			key = m.Name
		}
		if key == "" {
			continue
		}
		out.Prices[key] = modelPrice{
			InputPerMtok:       m.Cost.Text.Input.Price,
			CachedInputPerMtok: m.Cost.Text.Input.Cache.Read,
			OutputPerMtok:      m.Cost.Text.Output.Price,
		}
		out.GoMultipliers[key] = planMultiplier(m, "go", opencodeGoPrice)
		out.PlusMultipliers[key] = planMultiplier(m, "go-plus", opencodeGoPlusPrice)
	}
	return out
}

// planMultiplier converts list-price usage into a share of the subscription
// price: plan price / monthly usage allowance for that model. Exhausting a
// model's monthly allowance therefore adds up to the plan price.
func planMultiplier(m catalogModel, plan string, price float64) float64 {
	if m.Go == nil || price <= 0 {
		return 1
	}
	usage := m.Go.MonthlyUsage[plan]
	if usage <= 0 {
		return 1
	}
	return math.Round(price/usage*1e6) / 1e6
}

func catalogPrices(c *catalog) map[string]modelPrice { return catalogData(c).Prices }

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "genplanpresets: "+format+"\n", args...)
	os.Exit(1)
}
