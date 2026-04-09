package main

import (
	"encoding/json"
	"fmt"
	"go/format"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type apiOperation struct {
	Name        string
	HTTPMethod  string
	Path        string
	PathParams  []string // original snake_case names from URL
	HasBody     bool     // spec declares a requestBody
	ReqBodyRef  string   // Go type name from $ref, empty if untyped/absent
	RespRef     string   // Go type name from $ref, empty if untyped/absent
	SuccessCode int      // single 2xx status code
	// ErrorCodes maps status codes to the error schema ref they produce.
	// Only populated for codes that have a typed schema.
	ErrorCodes map[int]string
	Summary    string
}


func generateClient(specData []byte, outFile, pkgName string) error {
	var spec map[string]any
	if err := json.Unmarshal(specData, &spec); err != nil {
		return fmt.Errorf("parsing spec: %w", err)
	}

	ops, err := extractOperations(spec)
	if err != nil {
		return err
	}
	src := renderClient(pkgName, ops)

	formatted, err := format.Source([]byte(src))
	if err != nil {
		return fmt.Errorf("formatting generated client: %w (source:\n%s)", err, src)
	}
	return os.WriteFile(outFile, formatted, 0o644)
}

func extractOperations(spec map[string]any) ([]apiOperation, error) {
	paths, _ := spec["paths"].(map[string]any)

	// Collect raw operation data with short names (all params stripped).
	type rawOp struct {
		path, httpMethod string
		op               map[string]any
	}
	var raw []rawOp
	shortNameCounts := map[string]int{}
	for path, pathItem := range paths {
		methods, _ := pathItem.(map[string]any)
		for httpMethod, opData := range methods {
			if httpMethod == "parameters" {
				continue
			}
			op, ok := opData.(map[string]any)
			if !ok {
				continue
			}
			raw = append(raw, rawOp{path, httpMethod, op})
			short := deriveMethodName(httpMethod, path, op, false)
			shortNameCounts[short]++
		}
	}

	// Build operations, using trailing param only where needed to disambiguate.
	var ops []apiOperation
	for _, r := range raw {
		summary, _ := r.op["summary"].(string)
		_, hasBody := r.op["requestBody"]

		successCode, err := extractSuccessCode(r.op, r.httpMethod, r.path)
		if err != nil {
			return nil, err
		}

		short := deriveMethodName(r.httpMethod, r.path, r.op, false)
		name := short
		if shortNameCounts[short] > 1 {
			name = deriveMethodName(r.httpMethod, r.path, r.op, true)
		}

		ops = append(ops, apiOperation{
			Name:        name,
			HTTPMethod:  strings.ToUpper(r.httpMethod),
			Path:        r.path,
			PathParams:  extractPathParams(r.path),
			HasBody:     hasBody,
			ReqBodyRef:  bodySchemaRef(spec, r.op),
			RespRef:     responseSchemaRef(spec, r.op),
			SuccessCode: successCode,
			ErrorCodes:  errorCodeMap(spec, r.op),
			Summary:     summary,
		})
	}

	sort.Slice(ops, func(i, j int) bool { return ops[i].Name < ops[j].Name })
	return ops, nil
}

// extractSuccessCode finds the single 2xx status code for an operation.
// Fails if there isn't exactly one.
func extractSuccessCode(op map[string]any, httpMethod, path string) (int, error) {
	responses, _ := op["responses"].(map[string]any)
	var codes []int
	for codeStr := range responses {
		code, err := strconv.Atoi(codeStr)
		if err != nil {
			continue
		}
		if code >= 200 && code < 300 {
			codes = append(codes, code)
		}
	}
	if len(codes) != 1 {
		return 0, fmt.Errorf("expected exactly one 2xx response for %s %s, got %v", httpMethod, path, codes)
	}
	return codes[0], nil
}

var pathParamRe = regexp.MustCompile(`\{(\w+)\}`)

func extractPathParams(path string) []string {
	matches := pathParamRe.FindAllStringSubmatch(path, -1)
	params := make([]string, len(matches))
	for i, m := range matches {
		params[i] = m[1]
	}
	return params
}

func deriveMethodName(httpMethod, path string, op map[string]any, keepTrailingParam bool) string {
	if opID, ok := op["operationId"].(string); ok && opID != "" {
		return toPascalCase(opID)
	}
	cleanPath := strings.TrimPrefix(path, "/v1/")
	cleanPath = strings.TrimPrefix(cleanPath, "/")
	segments := strings.Split(cleanPath, "/")
	var kept []string
	for i, seg := range segments {
		if m := pathParamRe.FindStringSubmatch(seg); m != nil {
			if keepTrailingParam && i == len(segments)-1 {
				kept = append(kept, m[1])
			}
		} else {
			kept = append(kept, seg)
		}
	}
	return toPascalCase(httpMethod) + toPascalCase(strings.Join(kept, "/"))
}

func toPascalCase(s string) string {
	var b strings.Builder
	upper := true
	for _, c := range s {
		if c == '_' || c == '/' || c == '-' || c == '.' {
			upper = true
		} else if upper {
			if c >= 'a' && c <= 'z' {
				c -= 'a' - 'A'
			}
			b.WriteRune(c)
			upper = false
		} else {
			b.WriteRune(c)
		}
	}
	return b.String()
}

func snakeToCamel(s string) string {
	var b strings.Builder
	upper := false
	for i, c := range s {
		if c == '_' {
			upper = true
		} else if upper || i == 0 {
			if i > 0 && c >= 'a' && c <= 'z' {
				c -= 'a' - 'A'
			}
			b.WriteRune(c)
			upper = false
		} else {
			b.WriteRune(c)
		}
	}
	return b.String()
}

func bodySchemaRef(spec, op map[string]any) string {
	rb, _ := op["requestBody"].(map[string]any)
	rb = resolveRef(spec, rb)
	return jsonContentSchemaRef(spec, rb)
}

func responseSchemaRef(spec, op map[string]any) string {
	responses, _ := op["responses"].(map[string]any)
	for _, code := range []string{"200", "201", "202"} {
		respNode, _ := responses[code].(map[string]any)
		if respNode == nil {
			continue
		}
		resolved := resolveRef(spec, respNode)
		// If the schema itself is a $ref to components/schemas, use that.
		if ref := jsonContentSchemaRef(spec, resolved); ref != "" {
			return ref
		}
		// Otherwise, if the response was a $ref to components/responses and
		// has JSON content, oapi-codegen generates a type named after the
		// response component (e.g. AsyncPredictOutput).
		if ref, _ := respNode["$ref"].(string); ref != "" && hasJSONContent(resolved) {
			parts := strings.Split(ref, "/")
			return parts[len(parts)-1]
		}
	}
	return ""
}

func hasJSONContent(node map[string]any) bool {
	if node == nil {
		return false
	}
	content, _ := node["content"].(map[string]any)
	_, ok := content["application/json"]
	return ok
}

// errorCodeMap builds a map from HTTP error status codes to their typed
// error schema ref. Only codes with a resolvable JSON schema are included.
func errorCodeMap(spec, op map[string]any) map[int]string {
	responses, _ := op["responses"].(map[string]any)
	result := map[int]string{}
	for codeStr, respRaw := range responses {
		code, err := strconv.Atoi(codeStr)
		if err != nil || code < 400 {
			continue
		}
		resp, _ := respRaw.(map[string]any)
		resp = resolveRef(spec, resp)
		if ref := jsonContentSchemaRef(spec, resp); ref != "" {
			result[code] = ref
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// resolveRef follows a single $ref if present, returning the resolved object.
func resolveRef(spec, node map[string]any) map[string]any {
	if node == nil {
		return nil
	}
	ref, _ := node["$ref"].(string)
	if ref == "" {
		return node
	}
	parts := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
	var cur any = spec
	for _, p := range parts {
		m, _ := cur.(map[string]any)
		if m == nil {
			return nil
		}
		cur = m[p]
	}
	resolved, _ := cur.(map[string]any)
	return resolved
}

// jsonContentSchemaRef extracts a schema $ref name from
// .content["application/json"].schema. Returns "" for inline/absent schemas.
func jsonContentSchemaRef(spec, node map[string]any) string {
	if node == nil {
		return ""
	}
	content, _ := node["content"].(map[string]any)
	appJSON, _ := content["application/json"].(map[string]any)
	schema, _ := appJSON["schema"].(map[string]any)
	ref, _ := schema["$ref"].(string)
	if ref == "" {
		return ""
	}
	parts := strings.Split(ref, "/")
	return parts[len(parts)-1]
}

func pathFmt(path string) string {
	return pathParamRe.ReplaceAllLiteralString(path, "%s")
}

func renderClient(pkgName string, ops []apiOperation) string {
	hasTypedResp := false
	hasNoResp := false
	for _, op := range ops {
		if op.RespRef != "" {
			hasTypedResp = true
		} else {
			hasNoResp = true
		}
	}

	// Collect unique error schema refs for typed error types.
	errorRefs := map[string]bool{}
	for _, op := range ops {
		for _, ref := range op.ErrorCodes {
			errorRefs[ref] = true
		}
	}
	sortedErrorRefs := make([]string, 0, len(errorRefs))
	for ref := range errorRefs {
		sortedErrorRefs = append(sortedErrorRefs, ref)
	}
	sort.Strings(sortedErrorRefs)

	imports := map[string]bool{
		"bytes":         true,
		"context":       true,
		"encoding/json": true,
		"fmt":           true,
		"io":            true,
		"net/http":      true,
		"net/url":       true,
		"strings":       true,
	}

	var w strings.Builder
	pf := func(f string, a ...any) { fmt.Fprintf(&w, f, a...) }

	pf("// Code generated by apigen/clientgen. DO NOT EDIT.\n\n")

	pf("// Package %s is a generated client for the Baseten %s.\n", pkgName, pkgName)
	pf("//\n")
	pf("// Types and methods in this package are generated from the OpenAPI\n")
	pf("// specification and are NOT covered by any stability or compatibility\n")
	pf("// guarantees. They may change without notice between versions.\n")
	pf(`package %s

import (
`, pkgName)
	sortedImports := make([]string, 0, len(imports))
	for imp := range imports {
		sortedImports = append(sortedImports, imp)
	}
	sort.Strings(sortedImports)
	for _, imp := range sortedImports {
		pf("\t%q\n", imp)
	}
	pf(")\n")

	pf(`
// Client is a generated HTTP client for the %s API.
//
// On HTTP failure, methods return [*ResponseError] unless documented
// otherwise on the method.
type Client struct {
	BaseURL    string
	HTTPClient interface{ Do(*http.Request) (*http.Response, error) }
	// Headers are added to every request.
	Headers http.Header
}

// ResponseError represents a non-success HTTP response whose body could not
// be decoded into a typed error.
type ResponseError struct {
	StatusCode int
	Body       string
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("baseten API error (HTTP %%d): %%s", e.StatusCode, e.Body)
}
`, pkgName)

	// Typed error types — one per unique error schema ref.
	for _, ref := range sortedErrorRefs {
		pf(`
// Response%s is returned for non-success HTTP responses whose body
// decoded as [%s].
type Response%s struct {
	StatusCode int
	%s         %s
}

func (e *Response%s) Error() string {
	b, _ := json.Marshal(e.%s)
	return fmt.Sprintf("baseten API error (HTTP %%d): %%s", e.StatusCode, string(b))
}
`, ref, ref, ref, ref, ref, ref, ref)
	}

	// Methods.
	for _, op := range ops {
		pf("\n")
		renderMethod(&w, op)
	}

	// Internal request struct and helpers at bottom.
	pf(`
// errorType identifies a typed error schema for status-code-based dispatch.
type errorType int

const (
	errorTypeNone errorType = iota`)
	for i, ref := range sortedErrorRefs {
		_ = i
		pf("\n\terrorType%s", ref)
	}
	pf(`
)

type apiRequest struct {
	method      string
	pathFmt     string
	pathArgs    []any
	body        any
	successCode int
	// errorCodes maps HTTP status codes to a typed error schema. Status codes
	// not in this map (or decode failures) fall back to [*ResponseError].
	errorCodes  map[int]errorType
}

func (c *Client) do(ctx context.Context, r apiRequest) (*http.Response, error) {
	for i, p := range r.pathArgs {
		r.pathArgs[i] = url.PathEscape(fmt.Sprint(p))
	}
	path := fmt.Sprintf(r.pathFmt, r.pathArgs...)
	var bodyReader io.Reader
	if r.body != nil {
		b, err := json.Marshal(r.body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, c.BaseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, vals := range c.Headers {
		for _, val := range vals {
			req.Header.Add(key, val)
		}
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != r.successCode {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if et, ok := r.errorCodes[resp.StatusCode]; ok {
			if typedErr := decodeErrorType(et, resp.StatusCode, body); typedErr != nil {
				return nil, typedErr
			}
		}
		return nil, &ResponseError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	return resp, nil
}

func decodeErrorType(et errorType, statusCode int, body []byte) error {
	switch et {`)
	for _, ref := range sortedErrorRefs {
		pf(`
	case errorType%s:
		var detail %s
		if err := json.Unmarshal(body, &detail); err == nil {
			return &Response%s{StatusCode: statusCode, %s: detail}
		}`, ref, ref, ref, ref)
	}
	pf(`
	}
	return nil
}
`)

	if hasTypedResp {
		pf(`
func doJSON[T any](c *Client, ctx context.Context, r apiRequest) (*T, error) {
	resp, err := c.do(ctx, r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return nil, fmt.Errorf("unexpected content type %%q, expected application/json", ct)
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var result T
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
`)
	}

	if hasNoResp {
		pf(`
func (c *Client) doNoResponse(ctx context.Context, r apiRequest) error {
	resp, err := c.do(ctx, r)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
`)
	}

	return w.String()
}

func renderMethod(w *strings.Builder, op apiOperation) {
	pf := func(f string, a ...any) { fmt.Fprintf(w, f, a...) }

	// Signature params.
	params := []string{"ctx context.Context"}
	for _, p := range op.PathParams {
		params = append(params, snakeToCamel(p)+" string")
	}
	if op.HasBody {
		if op.ReqBodyRef != "" {
			params = append(params, "body "+op.ReqBodyRef)
		} else {
			params = append(params, "body any")
		}
	}

	// Doc comment.
	pf("// %s", op.Name)
	if op.Summary != "" {
		pf(": %s", op.Summary)
	}
	pf("\n")

	// Document typed errors per status code.
	if len(op.ErrorCodes) > 0 {
		pf("//\n")
		// Group codes by error ref.
		refCodes := map[string][]int{}
		for code, ref := range op.ErrorCodes {
			refCodes[ref] = append(refCodes[ref], code)
		}
		for ref, codes := range refCodes {
			sort.Ints(codes)
			codeStrs := make([]string, len(codes))
			for i, c := range codes {
				codeStrs[i] = strconv.Itoa(c)
			}
			pf("// Returns [*Response%s] on HTTP %s.\n", ref, strings.Join(codeStrs, ", "))
		}
	}

	// Body arg.
	bodyArg := "nil"
	if op.HasBody {
		bodyArg = "body"
	}

	// Path args.
	var pathArgsList string
	if len(op.PathParams) > 0 {
		args := make([]string, len(op.PathParams))
		for i, p := range op.PathParams {
			args[i] = snakeToCamel(p)
		}
		pathArgsList = "[]any{" + strings.Join(args, ", ") + "}"
	} else {
		pathArgsList = "nil"
	}

	// Error codes map.
	errorCodesExpr := "nil"
	if len(op.ErrorCodes) > 0 {
		codes := make([]int, 0, len(op.ErrorCodes))
		for c := range op.ErrorCodes {
			codes = append(codes, c)
		}
		sort.Ints(codes)
		var entries []string
		for _, c := range codes {
			entries = append(entries, fmt.Sprintf("%d: errorType%s", c, op.ErrorCodes[c]))
		}
		errorCodesExpr = "map[int]errorType{" + strings.Join(entries, ", ") + "}"
	}

	reqLit := fmt.Sprintf(`apiRequest{
		method:      %q,
		pathFmt:     %q,
		pathArgs:    %s,
		body:        %s,
		successCode: %d,
		errorCodes:  %s,
	}`, op.HTTPMethod, pathFmt(op.Path), pathArgsList, bodyArg, op.SuccessCode, errorCodesExpr)

	if op.RespRef != "" {
		pf("func (c *Client) %s(%s) (*%s, error) {\n", op.Name, strings.Join(params, ", "), op.RespRef)
		pf("\treturn doJSON[%s](c, ctx, %s)\n", op.RespRef, reqLit)
	} else {
		pf("func (c *Client) %s(%s) error {\n", op.Name, strings.Join(params, ", "))
		pf("\treturn c.doNoResponse(ctx, %s)\n", reqLit)
	}

	pf("}\n")
}
