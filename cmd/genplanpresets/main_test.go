package main

import (
	"encoding/json"
	"testing"
)

func TestCatalogData(t *testing.T) {
	var c catalog
	if err := json.Unmarshal([]byte(`{"data":[
		{"id":"chat-model","type":"chat","cost":{"text":{"input":{"price":1.5,"cache":{"read":0.15}},"output":{"price":2.5}}},"go":{"monthly_usage":{"go":100,"go-plus":200}}},
		{"id":"free-model","type":"chat","cost":{"text":{"input":{"price":0},"output":{"price":0}}}},
		{"id":"image-model","type":"image","cost":{"text":{"input":{"price":9},"output":{"price":9}}}},
		{"id":"no-cost","type":"chat"}
	]}`), &c); err != nil {
		t.Fatal(err)
	}
	data := catalogData(&c)
	if len(data.Prices) != 2 {
		t.Fatalf("prices = %+v", data.Prices)
	}
	if data.Prices["chat-model"].InputPerMtok != 1.5 || data.Prices["chat-model"].CachedInputPerMtok != 0.15 || data.Prices["chat-model"].OutputPerMtok != 2.5 {
		t.Fatalf("chat-model = %+v", data.Prices["chat-model"])
	}
	if _, ok := data.Prices["image-model"]; ok {
		t.Fatal("image model should be skipped")
	}
	if got := data.GoMultipliers["chat-model"]; got != 0.1 {
		t.Fatalf("go multiplier = %v, want 0.1", got)
	}
	if got := data.PlusMultipliers["chat-model"]; got != 0.2 {
		t.Fatalf("go-plus multiplier = %v, want 0.2", got)
	}
	if got := data.GoMultipliers["free-model"]; got != 1 {
		t.Fatalf("free multiplier = %v, want 1", got)
	}
}
