package steer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// SchemaInstruction is the contract appended to an agent fn's prompt when
// its return type is a declared record: the one place the type system
// speaks to the model.
func SchemaInstruction(t *RecordType, isList bool) string {
	fields := make([]string, len(t.Fields))
	for i, f := range t.Fields {
		fields[i] = fmt.Sprintf("%q: %s", f.Name, jsonTypeName(f.Type))
	}
	shape := "{" + strings.Join(fields, ", ") + "}"
	if isList {
		return fmt.Sprintf(
			"Respond ONLY with a JSON array of %s objects, each shaped exactly like %s. No prose, no code fences, no keys beyond the schema unless asked.",
			t.Name, shape)
	}
	return fmt.Sprintf(
		"Respond ONLY with a single JSON %s object shaped exactly like %s. No prose, no code fences.",
		t.Name, shape)
}

func jsonTypeName(fieldType string) string {
	switch fieldType {
	case "Int":
		return "integer"
	case "Bool":
		return "boolean"
	default:
		return "string"
	}
}

// ValidateRecord checks a model response against a record schema and
// returns the normalized (re-marshaled) JSON on success. Markdown code
// fences are tolerated and stripped — models add them no matter what the
// prompt says. The error text is written to be fed back to the model on
// the bounded retry.
func ValidateRecord(t *RecordType, raw string, isList bool) (string, error) {
	cleaned := stripCodeFences(strings.TrimSpace(raw))
	var v any
	if err := json.Unmarshal([]byte(cleaned), &v); err != nil {
		return "", fmt.Errorf("response is not valid JSON: %v", err)
	}
	if isList {
		arr, ok := v.([]any)
		if !ok {
			return "", fmt.Errorf("expected a JSON array of %s objects, got a %s", t.Name, jsonKind(v))
		}
		for i, el := range arr {
			if err := checkRecordObject(t, el); err != nil {
				return "", fmt.Errorf("element %d: %v", i+1, err)
			}
		}
	} else {
		if err := checkRecordObject(t, v); err != nil {
			return "", err
		}
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func checkRecordObject(t *RecordType, v any) error {
	obj, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("expected a JSON object shaped like %s, got a %s", t.Name, jsonKind(v))
	}
	var problems []string
	for _, f := range t.Fields {
		fv, present := obj[f.Name]
		if !present {
			problems = append(problems, fmt.Sprintf("missing field %q", f.Name))
			continue
		}
		switch f.Type {
		case "Int":
			n, ok := fv.(float64)
			if !ok || n != float64(int64(n)) {
				problems = append(problems, fmt.Sprintf("field %q must be an integer", f.Name))
			}
		case "Bool":
			if _, ok := fv.(bool); !ok {
				problems = append(problems, fmt.Sprintf("field %q must be a boolean", f.Name))
			}
		default: // Text
			if _, ok := fv.(string); !ok {
				problems = append(problems, fmt.Sprintf("field %q must be a string", f.Name))
			}
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func jsonKind(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	}
	return "value"
}

// stripCodeFences removes a single wrapping ``` or ```json fence.
func stripCodeFences(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	body := s[3:]
	if nl := strings.IndexByte(body, '\n'); nl >= 0 {
		// drop the info string ("json") on the opening fence line
		if lang := strings.TrimSpace(body[:nl]); lang == "" || !strings.ContainsAny(lang, "{}[]") {
			body = body[nl+1:]
		}
	}
	if i := strings.LastIndex(body, "```"); i >= 0 {
		body = body[:i]
	}
	return strings.TrimSpace(body)
}
