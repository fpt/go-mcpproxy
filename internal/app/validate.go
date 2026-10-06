package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// compileInputSchema compiles a tool's input schema. A nil schema with nil
// error means the tool has no schema worth validating against.
func compileInputSchema(tool mcp.Tool) (*jsonschema.Schema, error) {
	raw, err := inputSchemaJSON(tool)
	if err != nil || raw == nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode input schema of %q: %w", tool.Name, err)
	}
	url := "mem:///mcpproxy/" + tool.Name + ".json"
	c := jsonschema.NewCompiler()
	if err := c.AddResource(url, doc); err != nil {
		return nil, fmt.Errorf("register input schema of %q: %w", tool.Name, err)
	}
	return c.Compile(url)
}

func inputSchemaJSON(tool mcp.Tool) ([]byte, error) {
	if len(tool.RawInputSchema) > 0 {
		return tool.RawInputSchema, nil
	}
	s := tool.InputSchema
	if s.Type == "" && len(s.Properties) == 0 && len(s.Required) == 0 &&
		s.AdditionalProperties == nil {
		return nil, nil
	}
	return json.Marshal(s)
}

// validateArguments checks raw call arguments against a compiled schema.
func validateArguments(schema *jsonschema.Schema, req mcp.CallToolRequest) error {
	var raw []byte
	if len(bytes.TrimSpace(req.Params.RawArguments)) > 0 {
		raw = req.Params.RawArguments
	} else {
		var err error
		if raw, err = json.Marshal(req.Params.Arguments); err != nil {
			return fmt.Errorf("encode arguments: %w", err)
		}
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		raw = []byte("{}")
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("arguments are not valid JSON: %w", err)
	}
	if err := schema.Validate(value); err != nil {
		return flattenValidationError(err)
	}
	return nil
}

// flattenValidationError renders leaf violations as "<pointer>: <message>"
// lines, which are easier for an LLM to act on than the nested tree.
func flattenValidationError(err error) error {
	var verr *jsonschema.ValidationError
	if !errors.As(err, &verr) {
		return err
	}
	printer := message.NewPrinter(language.English)
	var parts []string
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			loc := "/" + strings.Join(e.InstanceLocation, "/")
			parts = append(parts, fmt.Sprintf("%s: %s", loc, e.ErrorKind.LocalizedString(printer)))
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(verr)
	return errors.New(strings.Join(parts, "; "))
}
