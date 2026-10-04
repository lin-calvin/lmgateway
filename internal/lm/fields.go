package lm

import (
	"fmt"
	"strings"
)

func chatGet(raw map[string]any, path string) (any, bool) {
	switch path {
	case "type":
		return "openai", true
	case "reasoning.effort":
		value, ok := raw["reasoning_effort"]
		return value, ok
	case "reasoning.summary":
		value, ok := raw["reasoning_summary"]
		return value, ok
	case "max_output_tokens":
		value, ok := raw["max_tokens"]
		return value, ok
	default:
		return genericGet(raw, path, nil)
	}
}

func chatSet(raw map[string]any, path string, value any) error {
	if err := validateSet(path, value); err != nil {
		return err
	}
	switch path {
	case "type":
		return fmt.Errorf("type is read-only")
	case "reasoning.effort":
		raw["reasoning_effort"] = value
		return nil
	case "reasoning.summary":
		raw["reasoning_summary"] = value
		return nil
	case "max_output_tokens":
		path = "max_tokens"
	}
	return setMapPath(raw, path, value)
}

func chatDelete(raw map[string]any, path string) error {
	switch path {
	case "type":
		return fmt.Errorf("type is read-only")
	case "reasoning.effort":
		delete(raw, "reasoning_effort")
		return nil
	case "reasoning.summary":
		delete(raw, "reasoning_summary")
		return nil
	case "max_output_tokens":
		path = "max_tokens"
	}
	return deleteMapPath(raw, path)
}

func responsesGet(raw map[string]any, path string) (any, bool) {
	switch path {
	case "type":
		return "openai_response", true
	case "reasoning.effort", "reasoning.summary":
		parts := strings.Split(path, ".")
		return nestedValue(raw, parts[0], parts[1])
	case "max_tokens":
		value, ok := raw["max_output_tokens"]
		return value, ok
	default:
		return genericGet(raw, path, nil)
	}
}

func responsesSet(raw map[string]any, path string, value any) error {
	if err := validateSet(path, value); err != nil {
		return err
	}
	switch path {
	case "type":
		return fmt.Errorf("type is read-only")
	case "reasoning.effort", "reasoning.summary":
		parts := strings.Split(path, ".")
		return setNestedValue(raw, parts[0], parts[1], value)
	case "max_tokens":
		path = "max_output_tokens"
	}
	return setMapPath(raw, path, value)
}

func responsesDelete(raw map[string]any, path string) error {
	switch path {
	case "type":
		return fmt.Errorf("type is read-only")
	case "reasoning.effort", "reasoning.summary":
		parts := strings.Split(path, ".")
		return deleteNestedValue(raw, parts[0], parts[1])
	case "max_tokens":
		path = "max_output_tokens"
	}
	return deleteMapPath(raw, path)
}

func genericGet(raw map[string]any, path string, _ any) (any, bool) {
	if path == "" || path == "type" {
		return nil, false
	}
	if strings.HasPrefix(path, "raw.") {
		path = strings.TrimPrefix(path, "raw.")
	}
	return getMapPath(raw, path)
}

func genericSet(raw map[string]any, path string, value any) error {
	if path == "type" {
		return fmt.Errorf("type is read-only")
	}
	return setMapPath(raw, path, value)
}

func genericDelete(raw map[string]any, path string) error {
	if path == "type" {
		return fmt.Errorf("type is read-only")
	}
	return deleteMapPath(raw, path)
}

func getMapPath(raw map[string]any, path string) (any, bool) {
	if strings.HasPrefix(path, "raw.") {
		path = strings.TrimPrefix(path, "raw.")
	}
	parts := strings.Split(path, ".")
	var current any = raw
	for _, part := range parts {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func setMapPath(raw map[string]any, path string, value any) error {
	if strings.HasPrefix(path, "raw.") {
		path = strings.TrimPrefix(path, "raw.")
	}
	if path == "" {
		return fmt.Errorf("empty document path")
	}
	parts := strings.Split(path, ".")
	current := raw
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			current[part] = next
		}
		current = next
	}
	current[parts[len(parts)-1]] = value
	return nil
}

func deleteMapPath(raw map[string]any, path string) error {
	if strings.HasPrefix(path, "raw.") {
		path = strings.TrimPrefix(path, "raw.")
	}
	if path == "" {
		return nil
	}
	parts := strings.Split(path, ".")
	if len(parts) == 1 {
		delete(raw, path)
		return nil
	}
	current := raw
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(map[string]any)
		if !ok {
			return nil
		}
		current = next
	}
	delete(current, parts[len(parts)-1])
	return nil
}

func nestedValue(raw map[string]any, parent, child string) (any, bool) {
	object, ok := raw[parent].(map[string]any)
	if !ok {
		return nil, false
	}
	value, ok := object[child]
	return value, ok
}

func setNestedValue(raw map[string]any, parent, child string, value any) error {
	object, ok := raw[parent].(map[string]any)
	if !ok {
		object = map[string]any{}
		raw[parent] = object
	}
	object[child] = value
	return nil
}

func deleteNestedValue(raw map[string]any, parent, child string) error {
	object, ok := raw[parent].(map[string]any)
	if !ok {
		return nil
	}
	delete(object, child)
	if len(object) == 0 {
		delete(raw, parent)
	}
	return nil
}

// isSet reports whether a document field carries a meaningful value. Absent,
// nil and empty-string values count as unset so defaults may fill them.
func isSet(value any, ok bool) bool {
	if !ok || value == nil {
		return false
	}
	if text, isText := value.(string); isText {
		return text != ""
	}
	return true
}

// applyDefault writes value only when the document field is unset.
func applyDefault(doc LMDocument, path string, value any) error {
	if current, ok := doc.Get(path); isSet(current, ok) {
		return nil
	}
	return doc.Set(path, value)
}

func validateSet(path string, value any) error {
	if strings.HasPrefix(path, "raw.") {
		return nil
	}
	switch path {
	case "model", "reasoning.effort", "reasoning.summary":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("field %s requires string", path)
		}
	case "stream":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("field %s requires bool", path)
		}
	case "temperature", "top_p", "max_tokens", "max_output_tokens":
		switch value.(type) {
		case int, int32, int64, float32, float64:
		default:
			return fmt.Errorf("field %s requires number", path)
		}
	}
	return nil
}
