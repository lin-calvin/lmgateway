package api

import (
	"encoding/json"

	"github.com/evanphx/json-patch/v5"
	"github.com/invopop/jsonschema"
	jsonschemav5 "github.com/santhosh-tekuri/jsonschema/v5"
)

// schemaBundle 一个 Go struct 生成的 JSON Schema（shape 校验）
type schemaBundle struct {
	compiled *jsonschemav5.Schema
}

func mustSchema(docType any) *schemaBundle {
	r := &jsonschema.Reflector{}
	schema := r.Reflect(docType)
	b, err := json.Marshal(schema)
	if err != nil {
		panic(err)
	}
	compiled, err := jsonschemav5.CompileString("schema.json", string(b))
	if err != nil {
		panic(err)
	}
	return &schemaBundle{compiled: compiled}
}

// Validate 校验一个 struct 实例的 shape
func (s *schemaBundle) Validate(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	return s.compiled.Validate(doc)
}

// mergePatch RFC 7386 JSON Merge Patch
func mergePatch(base, patch []byte) ([]byte, error) {
	return jsonpatch.MergePatch(base, patch)
}
