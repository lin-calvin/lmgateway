package lm

import (
	"fmt"
	"reflect"
)

type LMDocument interface {
	Get(path string) (any, bool)
	Set(path string, value any) error
	// SetDefault writes value only when the field is unset (absent, nil or
	// empty string), so explicit client values win over server defaults.
	SetDefault(path string, value any) error
	Delete(path string) error
	ConvertFrom(src LMDocument) (LMDocument, error)
}

type OpenAIChatRequest map[string]any
type OpenAIChatResponse map[string]any
type OpenAIResponseRequest map[string]any
type OpenAIResponseResponse map[string]any
type AnthropicRequest map[string]any
type AnthropicResponse map[string]any

type documentInfo struct {
	name      string
	direction bool
}

type documentPair struct {
	requestType  reflect.Type
	responseType reflect.Type
}

type Registry struct {
	byName   map[string]documentPair
	byType   map[reflect.Type]documentInfo
	provider map[string]string
}

var documentInterface = reflect.TypeOf((*LMDocument)(nil)).Elem()

func NewRegistry() *Registry {
	return &Registry{
		byName:   map[string]documentPair{},
		byType:   map[reflect.Type]documentInfo{},
		provider: map[string]string{},
	}
}

func (r *Registry) Register(name string, request, response any) error {
	requestType := reflect.TypeOf(request)
	responseType := reflect.TypeOf(response)
	if requestType == nil || responseType == nil {
		return fmt.Errorf("document %q has nil type", name)
	}
	if requestType.Kind() != reflect.Map || responseType.Kind() != reflect.Map {
		return fmt.Errorf("document %q must use map types", name)
	}
	if !requestType.Implements(documentInterface) || !responseType.Implements(documentInterface) {
		return fmt.Errorf("document %q types must implement LMDocument", name)
	}
	r.byName[name] = documentPair{requestType: requestType, responseType: responseType}
	r.byType[requestType] = documentInfo{name: name}
	r.byType[responseType] = documentInfo{name: name, direction: true}
	return nil
}

func (r *Registry) RegisterProvider(providerType, documentType string) error {
	if _, ok := r.byName[documentType]; !ok {
		return fmt.Errorf("unknown document type %q", documentType)
	}
	r.provider[providerType] = documentType
	return nil
}

func (r *Registry) NewRequest(name string, raw map[string]any) (LMDocument, error) {
	pair, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("unknown request document type %q", name)
	}
	// OpenAI-compatible clients commonly send unset optional fields as null.
	// Treat those fields as absent so adapters do not validate null as a typed
	// option such as max_output_tokens.
	return wrapMap(pair.requestType, omitNilFields(raw))
}

func omitNilFields(raw map[string]any) map[string]any {
	if raw == nil {
		return nil
	}
	out := make(map[string]any, len(raw))
	hasNil := false
	for key, value := range raw {
		if value != nil {
			out[key] = value
		} else {
			hasNil = true
		}
	}
	if !hasNil {
		return raw
	}
	return out
}

func (r *Registry) NewResponse(name string, raw map[string]any) (LMDocument, error) {
	pair, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("unknown response document type %q", name)
	}
	return wrapMap(pair.responseType, raw)
}

func wrapMap(typeOf reflect.Type, raw map[string]any) (LMDocument, error) {
	if raw == nil {
		raw = map[string]any{}
	}
	value := reflect.ValueOf(raw)
	if !value.Type().ConvertibleTo(typeOf) {
		return nil, fmt.Errorf("cannot wrap map as %s", typeOf)
	}
	doc, ok := value.Convert(typeOf).Interface().(LMDocument)
	if !ok {
		return nil, fmt.Errorf("document type %s does not implement LMDocument", typeOf)
	}
	return doc, nil
}

func (r *Registry) TypeOf(doc LMDocument) (string, bool) {
	if doc == nil {
		return "", false
	}
	if response, ok := doc.(*StreamResponse); ok {
		return response.protocol, response.protocol != ""
	}
	info, ok := r.byType[reflect.TypeOf(doc)]
	return info.name, ok
}

func (r *Registry) IsRequest(doc LMDocument) bool {
	if doc == nil {
		return false
	}
	if _, ok := doc.(*StreamResponse); ok {
		return false
	}
	info, ok := r.byType[reflect.TypeOf(doc)]
	return ok && !info.direction
}

func (r *Registry) IsResponse(doc LMDocument) bool {
	if doc == nil {
		return false
	}
	if _, ok := doc.(*StreamResponse); ok {
		return true
	}
	info, ok := r.byType[reflect.TypeOf(doc)]
	return ok && info.direction
}

func (r *Registry) Map(doc LMDocument) (map[string]any, bool) {
	if doc == nil {
		return nil, false
	}
	if response, ok := doc.(*StreamResponse); ok {
		return r.Map(response.document)
	}
	if _, ok := r.byType[reflect.TypeOf(doc)]; !ok {
		return nil, false
	}
	value := reflect.ValueOf(doc)
	rawType := reflect.TypeOf(map[string]any{})
	if !value.Type().ConvertibleTo(rawType) {
		return nil, false
	}
	return value.Convert(rawType).Interface().(map[string]any), true
}

func (r *Registry) ProviderDocumentType(providerType string) (string, bool) {
	name, ok := r.provider[providerType]
	return name, ok
}

func (r *Registry) Clone(doc LMDocument) (LMDocument, error) {
	name, ok := r.TypeOf(doc)
	if !ok {
		return nil, fmt.Errorf("cannot clone unknown document type %s", documentName(doc))
	}
	raw, ok := r.Map(doc)
	if !ok {
		return nil, fmt.Errorf("cannot clone document type %s", name)
	}
	copy := cloneMap(raw)
	if r.IsRequest(doc) {
		return r.NewRequest(name, copy)
	}
	if r.IsResponse(doc) {
		return r.NewResponse(name, copy)
	}
	return nil, fmt.Errorf("cannot clone document type %s", name)
}

func cloneMap(raw map[string]any) map[string]any {
	out := make(map[string]any, len(raw))
	for key, value := range raw {
		out[key] = cloneValue(value)
	}
	return out
}

func cloneValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneMap(value)
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = cloneValue(item)
		}
		return out
	case []string:
		return append([]string(nil), value...)
	default:
		return value
	}
}

var DefaultRegistry = NewRegistry()

func init() {
	registrations := []struct {
		name     string
		request  any
		response any
	}{
		{name: "openai", request: OpenAIChatRequest{}, response: OpenAIChatResponse{}},
		{name: "openai_response", request: OpenAIResponseRequest{}, response: OpenAIResponseResponse{}},
		{name: "anthropic", request: AnthropicRequest{}, response: AnthropicResponse{}},
	}
	for _, registration := range registrations {
		if err := DefaultRegistry.Register(registration.name, registration.request, registration.response); err != nil {
			panic(err)
		}
	}
	for providerType, documentType := range map[string]string{
		"openai":          "openai",
		"openai_response": "openai_response",
		"chatgpt_codex":   "openai_response",
		"anthropic":       "anthropic",
	} {
		if err := DefaultRegistry.RegisterProvider(providerType, documentType); err != nil {
			panic(err)
		}
	}
}

func TypeOf(doc LMDocument) (string, bool)      { return DefaultRegistry.TypeOf(doc) }
func IsRequest(doc LMDocument) bool             { return DefaultRegistry.IsRequest(doc) }
func IsResponse(doc LMDocument) bool            { return DefaultRegistry.IsResponse(doc) }
func Map(doc LMDocument) (map[string]any, bool) { return DefaultRegistry.Map(doc) }
func NewRequest(name string, raw map[string]any) (LMDocument, error) {
	return DefaultRegistry.NewRequest(name, raw)
}
func NewResponse(name string, raw map[string]any) (LMDocument, error) {
	return DefaultRegistry.NewResponse(name, raw)
}
func ProviderDocumentType(providerType string) (string, bool) {
	return DefaultRegistry.ProviderDocumentType(providerType)
}

func Clone(doc LMDocument) (LMDocument, error) {
	return DefaultRegistry.Clone(doc)
}

func documentName(doc LMDocument) string {
	if name, ok := TypeOf(doc); ok {
		return name
	}
	return "unknown"
}
