package main

import (
	"fmt"
	"sort"
	"strings"
)

type headerParameter struct {
	Name     string
	Field    string
	Required bool
	Array    bool
	DateTime bool
}

// Operation declarations override inherited headers. HTTP header names are
// case insensitive. Only header parameters participate, never cookie fields.
func extractHeaderParams(spec, paths map[string]any, path string, operation map[string]any) []headerParameter {
	byName := map[string]headerParameter{}
	for _, node := range []map[string]any{mapNode(paths[path]), operation} {
		parameters, _ := node["parameters"].([]any)
		for _, raw := range parameters {
			parameter := resolveRef(spec, mapNode(raw))
			if parameter["in"] != "header" {
				continue
			}
			name, _ := parameter["name"].(string)
			field := propertyGoFieldName(name)
			if override, ok := parameter["x-go-name"].(string); ok {
				field = override
			}
			required, _ := parameter["required"].(bool)
			schema := resolveRef(spec, mapNode(parameter["schema"]))
			byName[strings.ToLower(name)] = headerParameter{Name: name, Field: field, Required: required, Array: schema["type"] == "array", DateTime: schema["format"] == "date-time"}
		}
	}
	keys := []string{}
	for key := range byName {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]headerParameter, 0, len(keys))
	for _, key := range keys {
		result = append(result, byName[key])
	}
	return result
}

func renderHeaderParams(op apiOperation) string {
	if len(op.HeaderParams) == 0 {
		return ""
	}
	var code strings.Builder
	code.WriteString("headers := http.Header{}\n")
	for _, header := range op.HeaderParams {
		code.WriteString("{\n")
		value := "params." + header.Field
		if !header.Required {
			fmt.Fprintf(&code, "if %s != nil {\n", value)
			value = "*" + value
		}
		if header.Array {
			fmt.Fprintf(&code, "values := make([]string, 0, len(%s));for _,value := range %s { values=append(values,fmt.Sprint(value)) };headers.Set(%q,strings.Join(values,\",\"))\n", value, value, header.Name)
		} else if header.DateTime {
			fmt.Fprintf(&code, "headers.Set(%q,(%s).Format(time.RFC3339))\n", header.Name, value)
		} else {
			fmt.Fprintf(&code, "headers.Set(%q,fmt.Sprint(%s))\n", header.Name, value)
		}
		if !header.Required {
			code.WriteString("}\n")
		}
		code.WriteString("}\n")
	}
	return code.String()
}
