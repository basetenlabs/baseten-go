package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

func mapNode(v any) map[string]any { m, _ := v.(map[string]any); return m }
func normalizeSchemas(doc map[string]any) error {
	schemas := componentSchemas(doc)
	renames := map[string]string{}
	targets := map[string]string{}
	for name := range schemas {
		n := name
		if strings.ContainsAny(n, ".-") {
			n = toPascalCase(n)
		}
		canonical := strings.TrimSuffix(n, "V1")
		if prior, ok := targets[canonical]; ok && prior != name {
			return fmt.Errorf("schema collision: %q and %q", prior, name)
		}
		targets[canonical] = name
		if n != name {
			renames[name] = n
		}
	}
	var walk func(any)
	walk = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			for k, x := range n {
				if text, ok := x.(string); ok {
					for old, newName := range renames {
						if text == "#/components/schemas/"+old {
							n[k] = "#/components/schemas/" + newName
						}
					}
				} else {
					walk(x)
				}
			}
		case []any:
			for _, x := range n {
				walk(x)
			}
		}
	}
	walk(doc)
	for old, n := range renames {
		schemas[n] = schemas[old]
		delete(schemas, old)
	}
	return nil
}
func hoistInlineJSON(doc map[string]any) {
	components := mapNode(doc["components"])
	if components == nil {
		components = map[string]any{}
		doc["components"] = components
	}
	schemas := mapNode(components["schemas"])
	if schemas == nil {
		schemas = map[string]any{}
		components["schemas"] = schemas
	}
	paths := mapNode(doc["paths"])
	names := []string{}
	for path := range paths {
		names = append(names, path)
	}
	sort.Strings(names)
	for _, path := range names {
		item := mapNode(paths[path])
		for _, method := range []string{"get", "post", "put", "patch", "delete", "head", "options"} {
			op := mapNode(item[method])
			if op == nil {
				continue
			}
			name := strings.TrimSuffix(oapiParamsTypeName(method, path), "Params")
			hoist := func(node map[string]any, suffix string) {
				media := mapNode(mapNode(node["content"])["application/json"])
				schema := mapNode(media["schema"])
				if schema == nil || schema["$ref"] != nil {
					return
				}
				candidate := name + suffix
				for i := 2; schemas[candidate] != nil; i++ {
					candidate = name + suffix + strconv.Itoa(i)
				}
				schemas[candidate] = schema
				media["schema"] = map[string]any{"$ref": "#/components/schemas/" + candidate}
			}
			hoist(resolveRef(doc, mapNode(op["requestBody"])), "Body")
			responses := mapNode(op["responses"])
			codes := []string{}
			for code := range responses {
				codes = append(codes, code)
			}
			sort.Strings(codes)
			for _, code := range codes {
				hoist(resolveRef(doc, mapNode(responses[code])), "Response"+code)
			}
		}
	}
}
func successResponseRefs(spec, op map[string]any) map[int]string {
	result := map[int]string{}
	for status, raw := range mapNode(op["responses"]) {
		code, err := strconv.Atoi(status)
		if err != nil || code < 200 || code >= 300 {
			continue
		}
		result[code] = jsonContentSchemaRef(spec, resolveRef(spec, mapNode(raw)))
	}
	return result
}
func hasNonJSONResponse(spec, op map[string]any) bool {
	for status, raw := range mapNode(op["responses"]) {
		code, _ := strconv.Atoi(status)
		if code < 200 || code >= 300 {
			continue
		}
		for media := range mapNode(resolveRef(spec, mapNode(raw))["content"]) {
			if media != "application/json" {
				return true
			}
		}
	}
	return false
}
func requestContentType(spec, op map[string]any) string {
	content := mapNode(resolveRef(spec, mapNode(op["requestBody"]))["content"])
	if _, ok := content["application/json"]; ok {
		return "application/json"
	}
	types := []string{}
	for media := range content {
		types = append(types, media)
	}
	sort.Strings(types)
	if len(types) > 0 {
		return types[0]
	}
	return ""
}
func nonJSONResponseTypes(spec, op map[string]any) []string {
	types := map[string]bool{}
	for status, raw := range mapNode(op["responses"]) {
		code, _ := strconv.Atoi(status)
		if code < 200 || code >= 300 {
			continue
		}
		for media := range mapNode(resolveRef(spec, mapNode(raw))["content"]) {
			if media != "application/json" {
				types[media] = true
			}
		}
	}
	result := []string{}
	for media := range types {
		result = append(result, media)
	}
	sort.Strings(result)
	return result
}
func renderRawMethod(w *strings.Builder, op apiOperation) {
	params := []string{"ctx context.Context"}
	args := []string{}
	for _, p := range op.PathParams {
		params = append(params, snakeToCamel(p)+" string")
		args = append(args, snakeToCamel(p))
	}
	query := ""
	headerField := ""
	if len(op.HeaderParams) > 0 {
		headerField = "headers: headers,"
	}
	if op.ParamsType != "" {
		params = append(params, "params "+op.ParamsType)
		query = "queryParams: params,"
	}
	bodyField := ""
	if op.HasBody {
		bodyType := op.ReqBodyRef
		if bodyType == "" {
			bodyType = "any"
		}
		params = append(params, "body "+bodyType)
		bodyField = "body: body,"
	}
	params = append(params, "options RawRequestOptions")
	codes := []int{}
	for c := range op.ErrorCodes {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	errors := []string{}
	for _, c := range codes {
		errors = append(errors, fmt.Sprintf("%d: errorType%s", c, op.ErrorCodes[c]))
	}
	fmt.Fprintf(w, "\n// %sRaw returns the response unread in the requested content type. The caller must close the returned Body.\nfunc (c *Client) %sRaw(%s) (*http.Response,error) {%s return c.do(ctx,apiRequest{method:%q,pathFmt:%q,pathArgs:[]any{%s},%s accept:options.Accept,successCodes:%#v,errorCodes:map[int]errorType{%s}})}\n", op.Name, op.Name, strings.Join(params, ","), renderHeaderParams(op), op.HTTPMethod, pathFmt(op.Path), strings.Join(args, ","), query+headerField+bodyField, op.SuccessCodes, strings.Join(errors, ","))
}
func renderMultiMethod(w *strings.Builder, op apiOperation, params []string, req string) {
	fmt.Fprintf(w, "// %sResponse retains the status and decoded payload. Close Body if non-nil.\ntype %sResponse struct {StatusCode int;Header http.Header;Body io.ReadCloser\n", op.Name, op.Name)
	for _, code := range op.SuccessCodes {
		if ref := op.Responses[code]; ref != "" {
			fmt.Fprintf(w, "JSON%d *%s\n", code, ref)
		}
	}
	fmt.Fprintln(w, "}")
	fmt.Fprintf(w, "func(c *Client)%s(%s)(*%sResponse,error){%s resp,err:=c.do(ctx,%s);if err!=nil{return nil,err};result:=&%sResponse{StatusCode:resp.StatusCode,Header:resp.Header.Clone()};switch resp.StatusCode{\n", op.Name, strings.Join(params, ","), op.Name, renderHeaderParams(op), req, op.Name)
	for _, code := range op.SuccessCodes {
		if ref := op.Responses[code]; ref != "" {
			fmt.Fprintf(w, "case %d: defer resp.Body.Close();if ct:=resp.Header.Get(\"Content-Type\"); !strings.HasPrefix(ct,\"application/json\") { return nil,fmt.Errorf(\"unexpected content type %%q, expected application/json\",ct) };var value %s;if err:=json.NewDecoder(resp.Body).Decode(&value);err!=nil{return nil,err};result.JSON%d=&value\n", code, ref, code)
		}
	}
	fmt.Fprintln(w, "default:result.Body=resp.Body};return result,nil}")
}
