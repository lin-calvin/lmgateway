package lm

import "fmt"

func (r OpenAIChatRequest) Get(path string) (any, bool) { return chatGet(map[string]any(r), path) }
func (r OpenAIChatRequest) Set(path string, value any) error {
	return chatSet(map[string]any(r), path, value)
}
func (r OpenAIChatRequest) SetDefault(path string, value any) error {
	return applyDefault(r, path, value)
}
func (r OpenAIChatRequest) Delete(path string) error     { return chatDelete(map[string]any(r), path) }
func (r OpenAIChatResponse) Get(path string) (any, bool) { return chatGet(map[string]any(r), path) }
func (r OpenAIChatResponse) Set(path string, value any) error {
	return chatSet(map[string]any(r), path, value)
}
func (r OpenAIChatResponse) SetDefault(path string, value any) error {
	return applyDefault(r, path, value)
}
func (r OpenAIChatResponse) Delete(path string) error { return chatDelete(map[string]any(r), path) }
func (r OpenAIResponseRequest) Get(path string) (any, bool) {
	return responsesGet(map[string]any(r), path)
}
func (r OpenAIResponseRequest) Set(path string, value any) error {
	return responsesSet(map[string]any(r), path, value)
}
func (r OpenAIResponseRequest) SetDefault(path string, value any) error {
	return applyDefault(r, path, value)
}
func (r OpenAIResponseRequest) Delete(path string) error {
	return responsesDelete(map[string]any(r), path)
}
func (r OpenAIResponseResponse) Get(path string) (any, bool) {
	return responsesGet(map[string]any(r), path)
}
func (r OpenAIResponseResponse) Set(path string, value any) error {
	return responsesSet(map[string]any(r), path, value)
}
func (r OpenAIResponseResponse) SetDefault(path string, value any) error {
	return applyDefault(r, path, value)
}
func (r OpenAIResponseResponse) Delete(path string) error {
	return responsesDelete(map[string]any(r), path)
}
func (r AnthropicRequest) Get(path string) (any, bool) {
	return genericGet(map[string]any(r), path, nil)
}
func (r AnthropicRequest) Set(path string, value any) error {
	return genericSet(map[string]any(r), path, value)
}
func (r AnthropicRequest) SetDefault(path string, value any) error {
	return applyDefault(r, path, value)
}
func (r AnthropicRequest) Delete(path string) error { return genericDelete(map[string]any(r), path) }
func (r AnthropicResponse) Get(path string) (any, bool) {
	return genericGet(map[string]any(r), path, nil)
}
func (r AnthropicResponse) Set(path string, value any) error {
	return genericSet(map[string]any(r), path, value)
}
func (r AnthropicResponse) SetDefault(path string, value any) error {
	return applyDefault(r, path, value)
}
func (r AnthropicResponse) Delete(path string) error { return genericDelete(map[string]any(r), path) }

func (r OpenAIChatRequest) ConvertFrom(src LMDocument) (LMDocument, error) {
	if name, ok := TypeOf(src); ok && name == "openai" && IsRequest(src) {
		return src, nil
	}
	if name, ok := TypeOf(src); !ok || name != "openai_response" || !IsRequest(src) {
		return nil, fmt.Errorf("cannot convert %s to openai request", documentName(src))
	}
	return chatRequestFromResponses(r, src.(OpenAIResponseRequest))
}
func (r OpenAIChatResponse) ConvertFrom(src LMDocument) (LMDocument, error) {
	if name, ok := TypeOf(src); ok && name == "openai" && IsResponse(src) {
		return src, nil
	}
	if name, ok := TypeOf(src); !ok || name != "openai_response" || !IsResponse(src) {
		return nil, fmt.Errorf("cannot convert %s to openai response", documentName(src))
	}
	return chatResponseFromResponses(r, src.(OpenAIResponseResponse))
}
func (r OpenAIResponseRequest) ConvertFrom(src LMDocument) (LMDocument, error) {
	if name, ok := TypeOf(src); ok && name == "openai_response" && IsRequest(src) {
		return src, nil
	}
	if name, ok := TypeOf(src); !ok || name != "openai" || !IsRequest(src) {
		return nil, fmt.Errorf("cannot convert %s to openai responses request", documentName(src))
	}
	return responsesRequestFromChat(r, src.(OpenAIChatRequest))
}
func (r OpenAIResponseResponse) ConvertFrom(src LMDocument) (LMDocument, error) {
	if name, ok := TypeOf(src); ok && name == "openai_response" && IsResponse(src) {
		return src, nil
	}
	if name, ok := TypeOf(src); !ok || name != "openai" || !IsResponse(src) {
		return nil, fmt.Errorf("cannot convert %s to openai responses response", documentName(src))
	}
	return responsesResponseFromChat(r, src.(OpenAIChatResponse))
}
func (r AnthropicRequest) ConvertFrom(src LMDocument) (LMDocument, error) {
	if name, ok := TypeOf(src); ok && name == "anthropic" && IsRequest(src) {
		return src, nil
	}
	return nil, fmt.Errorf("cannot convert %s to anthropic request", documentName(src))
}
func (r AnthropicResponse) ConvertFrom(src LMDocument) (LMDocument, error) {
	if name, ok := TypeOf(src); ok && name == "anthropic" && IsResponse(src) {
		return src, nil
	}
	return nil, fmt.Errorf("cannot convert %s to anthropic response", documentName(src))
}
