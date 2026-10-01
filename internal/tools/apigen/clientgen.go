package main

import (
	"encoding/json"
	"fmt"
	"go/format"
	"maps"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

const jsonContentType = "application/json"

type apiOperation struct {
	Name       string
	HTTPMethod string
	Path       string
	PathParams []string // original snake_case names from URL
	HasBody    bool     // spec declares a requestBody
	// BodyContentType is the declared request body content type. Empty when
	// the operation has no body.
	BodyContentType string
	ReqBodyRef      string // Go type name from $ref, empty if untyped/absent or non-JSON
	// JSONResponses are the typed JSON success responses, ascending by status
	// code.
	JSONResponses []jsonResponse
	// RawAccepts are the non-JSON 2xx content types, e.g. text/plain or
	// application/octet-stream.
	RawAccepts   []string
	SuccessCodes []int // 2xx status codes, ascending
	// ErrorCodes maps status codes to the error schema ref they produce.
	// Only populated for codes that have a typed schema.
	ErrorCodes map[int]string
	Summary    string
	// ParamsType is the oapi-codegen-generated Params struct name for query
	// parameters, e.g. "GetV1BillingUsageSummaryParams". Empty if the
	// operation has no query parameters.
	ParamsType string
}

type jsonResponse struct {
	Code int
	Ref  string
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
	schemas := componentSchemas(spec)
	for _, op := range ops {
		if _, exists := schemas[op.Name+"Response"]; exists && len(op.JSONResponses) > 1 {
			return fmt.Errorf("result type %sResponse collides with an existing schema name", op.Name)
		}
	}
	src := renderClient(pkgName, ops)

	formatted, err := format.Source([]byte(src))
	if err != nil {
		return fmt.Errorf("formatting generated client: %w (source:\n%s)", err, src)
	}
	return os.WriteFile(outFile, formatted, 0o644)
}

// specOperation is one operation in a spec's paths.
type specOperation struct {
	path, httpMethod string
	op               map[string]any
}

func specOperations(spec map[string]any) []specOperation {
	paths, _ := spec["paths"].(map[string]any)
	var ops []specOperation
	for path, pathItem := range paths {
		methods, _ := pathItem.(map[string]any)
		for httpMethod, opData := range methods {
			if httpMethod == "parameters" {
				continue
			}
			if op, ok := opData.(map[string]any); ok {
				ops = append(ops, specOperation{path, httpMethod, op})
			}
		}
	}
	return ops
}

// methodKey identifies an operation in the map from resolveMethodNames.
func methodKey(httpMethod, path string) string {
	return httpMethod + " " + path
}

// resolveMethodNames returns the client method name for every operation, keyed
// by methodKey. Names drop path params, keeping the trailing one only where
// needed to disambiguate. Shared with preprocessing, which names hoisted
// schemas after their method.
func resolveMethodNames(spec map[string]any) map[string]string {
	ops := specOperations(spec)
	shortNameCounts := map[string]int{}
	for _, o := range ops {
		shortNameCounts[deriveMethodName(o.httpMethod, o.path, o.op, false)]++
	}
	names := map[string]string{}
	for _, o := range ops {
		name := deriveMethodName(o.httpMethod, o.path, o.op, false)
		if shortNameCounts[name] > 1 {
			name = deriveMethodName(o.httpMethod, o.path, o.op, true)
		}
		names[methodKey(o.httpMethod, o.path)] = name
	}
	return names
}

func extractOperations(spec map[string]any) ([]apiOperation, error) {
	paths, _ := spec["paths"].(map[string]any)
	names := resolveMethodNames(spec)

	var ops []apiOperation
	for _, o := range specOperations(spec) {
		summary, _ := o.op["summary"].(string)
		_, hasBody := o.op["requestBody"]

		successCodes := successStatusCodes(o.op)
		if len(successCodes) == 0 {
			return nil, fmt.Errorf("expected at least one 2xx response for %s %s", strings.ToUpper(o.httpMethod), o.path)
		}

		// A Raw method requests the one non-JSON content type, so several would
		// need a way for the caller to choose, which no spec has needed yet.
		rawAccepts := rawResponseAccepts(spec, o.op, successCodes)
		if len(rawAccepts) > 1 {
			return nil, fmt.Errorf("%s %s has several non-JSON response content types %v, Raw methods support one", strings.ToUpper(o.httpMethod), o.path, rawAccepts)
		}

		paramsType := ""
		if hasQueryParams(spec, paths, o.path, o.op) {
			paramsType = oapiParamsTypeName(o.httpMethod, o.path, o.op)
		}

		ops = append(ops, apiOperation{
			Name:            names[methodKey(o.httpMethod, o.path)],
			HTTPMethod:      strings.ToUpper(o.httpMethod),
			Path:            o.path,
			PathParams:      extractPathParams(o.path),
			HasBody:         hasBody,
			BodyContentType: bodyContentType(spec, o.op),
			ReqBodyRef:      bodySchemaRef(spec, o.op),
			JSONResponses:   jsonResponseRefs(spec, o.op, successCodes),
			RawAccepts:      rawAccepts,
			SuccessCodes:    successCodes,
			ErrorCodes:      errorCodeMap(spec, o.op),
			Summary:         summary,
			ParamsType:      paramsType,
		})
	}

	sort.Slice(ops, func(i, j int) bool { return ops[i].Name < ops[j].Name })
	return ops, nil
}

// successStatusCodes returns an operation's 2xx status codes, ascending.
func successStatusCodes(op map[string]any) []int {
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
	slices.Sort(codes)
	return codes
}

// hasQueryParams reports whether the operation has any `in: query` parameters,
// either declared on the operation itself or on the path item. $refs to
// components/parameters are resolved.
func hasQueryParams(spec map[string]any, paths map[string]any, path string, op map[string]any) bool {
	collect := func(node map[string]any) bool {
		raw, _ := node["parameters"].([]any)
		for _, p := range raw {
			pm, _ := p.(map[string]any)
			pm = resolveRef(spec, pm)
			if loc, _ := pm["in"].(string); loc == "query" {
				return true
			}
		}
		return false
	}
	if collect(op) {
		return true
	}
	pathItem, _ := paths[path].(map[string]any)
	return collect(pathItem)
}

// oapiParamsTypeName mirrors oapi-codegen's default naming for the Params
// struct emitted for an operation with parameters: from the operationId when
// set (ListSandboxes becomes ListSandboxesParams), otherwise from the method
// and path (GET /v1/billing/usage_summary becomes
// GetV1BillingUsageSummaryParams).
func oapiParamsTypeName(httpMethod, path string, op map[string]any) string {
	if opID, ok := op["operationId"].(string); ok && opID != "" {
		return toPascalCase(opID) + "Params"
	}
	cleanPath := strings.TrimPrefix(path, "/")
	segments := strings.Split(cleanPath, "/")
	var parts []string
	for _, seg := range segments {
		if m := pathParamRe.FindStringSubmatch(seg); m != nil {
			parts = append(parts, toPascalCase(m[1]))
		} else {
			parts = append(parts, toPascalCase(seg))
		}
	}
	return toPascalCase(httpMethod) + strings.Join(parts, "") + "Params"
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

// jsonResponseRefs returns the typed JSON success responses, one per 2xx code
// that declares a JSON body.
func jsonResponseRefs(spec, op map[string]any, successCodes []int) []jsonResponse {
	responses, _ := op["responses"].(map[string]any)
	var result []jsonResponse
	for _, code := range successCodes {
		respNode, _ := responses[strconv.Itoa(code)].(map[string]any)
		if respNode == nil {
			continue
		}
		resolved := resolveRef(spec, respNode)
		// If the schema itself is a $ref to components/schemas, use that.
		ref := jsonContentSchemaRef(spec, resolved)
		// Otherwise, if the response was a $ref to components/responses and
		// has JSON content, oapi-codegen generates a type named after the
		// response component (e.g. AsyncPredictOutput).
		if respRef, _ := respNode["$ref"].(string); ref == "" && respRef != "" && hasJSONContent(resolved) {
			parts := strings.Split(respRef, "/")
			ref = parts[len(parts)-1]
		}
		if ref != "" {
			result = append(result, jsonResponse{Code: code, Ref: ref})
		}
	}
	return result
}

// rawResponseAccepts returns the non-JSON content types declared on 2xx
// responses, deduplicated and sorted.
func rawResponseAccepts(spec, op map[string]any, successCodes []int) []string {
	responses, _ := op["responses"].(map[string]any)
	accepts := map[string]bool{}
	for _, code := range successCodes {
		respNode, _ := responses[strconv.Itoa(code)].(map[string]any)
		resolved := resolveRef(spec, respNode)
		content, _ := resolved["content"].(map[string]any)
		for contentType := range content {
			if contentType != jsonContentType {
				accepts[contentType] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(accepts))
}

// bodyContentType returns the declared request body content type, preferring
// JSON when an operation declares several encodings. Empty when the operation
// has no body.
func bodyContentType(spec, op map[string]any) string {
	rb, _ := op["requestBody"].(map[string]any)
	rb = resolveRef(spec, rb)
	content, _ := rb["content"].(map[string]any)
	if _, ok := content[jsonContentType]; ok {
		return jsonContentType
	}
	types := slices.Sorted(maps.Keys(content))
	if len(types) == 0 {
		return ""
	}
	return types[0]
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
	hasMultiResp := false
	hasNoResp := false
	for _, op := range ops {
		switch {
		case len(op.JSONResponses) == 1:
			hasTypedResp = true
		case len(op.JSONResponses) > 1:
			hasMultiResp = true
		case len(op.RawAccepts) == 0:
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

	hasQuery := false
	for _, op := range ops {
		if op.ParamsType != "" {
			hasQuery = true
			break
		}
	}

	imports := map[string]bool{
		"bytes":         true,
		"context":       true,
		"encoding/json": true,
		"fmt":           true,
		"io":            true,
		"net/http":      true,
		"net/url":       true,
		"slices":        true,
		"strings":       true,
	}
	hasRawBody := false
	for _, op := range ops {
		if op.HasBody && op.BodyContentType != jsonContentType && op.BodyContentType != "" {
			hasRawBody = true
			break
		}
	}
	if hasRawBody {
		imports["cmp"] = true
	}
	if hasQuery {
		imports["reflect"] = true
		imports["strconv"] = true
		imports["time"] = true
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

	if hasRawBody {
		pf(`
// AdvancedRequest is the request body for an operation whose body is not JSON.
type AdvancedRequest struct {
	// Body is sent as is.
	Body io.Reader
	// ContentType overrides the content type the operation declares. Required
	// for multipart/form-data, since it carries the boundary.
	ContentType string
}
`)
	}

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
		// An operation with no JSON success body returns the response directly,
		// since there is nothing to deserialize into.
		rawOnly := len(op.JSONResponses) == 0 && len(op.RawAccepts) > 0
		pf("\n")
		renderMethod(&w, op, rawOnly)
		// A content-negotiated operation also gets a sibling returning the raw
		// response, since Accept changes the body's type entirely.
		if len(op.JSONResponses) > 0 && len(op.RawAccepts) > 0 {
			pf("\n")
			renderMethod(&w, op, true)
		}
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
	queryField := ""
	queryEncode := ""
	if hasQuery {
		queryField = "\tqueryParams any\n"
		queryEncode = "\tif r.queryParams != nil {\n\t\tif q := encodeQuery(r.queryParams); len(q) > 0 {\n\t\t\tpath += \"?\" + q.Encode()\n\t\t}\n\t}\n"
	}
	pf(`
)

type apiRequest struct {
	method   string
	pathFmt  string
	pathArgs []any
%s	// body is marshaled as JSON. rawBody is sent as is, with bodyContentType.
	// At most one of them is set.
	body            any
	rawBody         io.Reader
	bodyContentType string
	// accept is the Accept header to send. Empty when the response is JSON.
	accept       string
	successCodes []int
	// errorCodes maps HTTP status codes to a typed error schema. Status codes
	// not in this map (or decode failures) fall back to [*ResponseError].
	errorCodes  map[int]errorType
}

func (c *Client) do(ctx context.Context, r apiRequest) (*http.Response, error) {
	for i, p := range r.pathArgs {
		r.pathArgs[i] = url.PathEscape(fmt.Sprint(p))
	}
	path := fmt.Sprintf(r.pathFmt, r.pathArgs...)
%s	bodyReader, contentType := r.rawBody, r.bodyContentType
	if r.body != nil {
		b, err := json.Marshal(r.body)
		if err != nil {
			return nil, err
		}
		bodyReader, contentType = bytes.NewReader(b), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, r.method, c.BaseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	for key, vals := range c.Headers {
		for _, val := range vals {
			req.Header.Add(key, val)
		}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if r.accept != "" {
		req.Header.Set("Accept", r.accept)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(r.successCodes, resp.StatusCode) {
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
	switch et {`, queryField, queryEncode)
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

	if hasQuery {
		pf(`
// encodeQuery walks a Params struct emitted by oapi-codegen and renders its
// fields as url.Values using the "form" struct tag. Pointer fields that are
// nil are skipped. time.Time values are rendered as RFC 3339. Slice fields
// (including named-element slices like []Enum) are exploded into one repeated
// query parameter per element.
func encodeQuery(p any) url.Values {
	q := url.Values{}
	v := reflect.ValueOf(p)
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return q
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return q
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("form")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.SplitN(tag, ",", 2)[0]
		if name == "" {
			continue
		}
		fv := v.Field(i)
		if fv.Kind() == reflect.Ptr {
			if fv.IsNil() {
				continue
			}
			fv = fv.Elem()
		}
		if fv.Kind() == reflect.Slice {
			for j := 0; j < fv.Len(); j++ {
				q.Add(name, fmt.Sprint(fv.Index(j).Interface()))
			}
			continue
		}
		switch x := fv.Interface().(type) {
		case time.Time:
			q.Set(name, x.Format(time.RFC3339))
		default:
			switch fv.Kind() {
			case reflect.String:
				q.Set(name, fv.String())
			case reflect.Bool:
				q.Set(name, strconv.FormatBool(fv.Bool()))
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				q.Set(name, strconv.FormatInt(fv.Int(), 10))
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				q.Set(name, strconv.FormatUint(fv.Uint(), 10))
			case reflect.Float32, reflect.Float64:
				q.Set(name, strconv.FormatFloat(fv.Float(), 'g', -1, 64))
			default:
				q.Set(name, fmt.Sprint(fv.Interface()))
			}
		}
	}
	return q
}
`)
	}

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

	if hasMultiResp {
		pf(`
// doJSONWithStatus reads a JSON body and pairs it with the status, for
// operations whose success statuses return different types.
func (c *Client) doJSONWithStatus(ctx context.Context, r apiRequest) (int, []byte, error) {
	resp, err := c.do(ctx, r)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return 0, nil, fmt.Errorf("unexpected content type %%q, expected application/json", ct)
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, respBody, nil
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

// renderMethod renders one method. With raw, it renders a method returning the
// response unread instead, named with a Raw suffix and requesting the
// operation's non-JSON content type.
func renderMethod(w *strings.Builder, op apiOperation, raw bool) {
	pf := func(f string, a ...any) { fmt.Fprintf(w, f, a...) }

	// Every method returning the response unread is suffixed, so the name alone
	// says the caller must close the body.
	name := op.Name
	if raw {
		name += "Raw"
	}

	jsonBody := op.BodyContentType == jsonContentType || op.BodyContentType == ""

	// Signature params.
	params := []string{"ctx context.Context"}
	for _, p := range op.PathParams {
		params = append(params, snakeToCamel(p)+" string")
	}
	if op.ParamsType != "" {
		params = append(params, "params "+op.ParamsType)
	}
	if op.HasBody {
		switch {
		case jsonBody && op.ReqBodyRef != "":
			params = append(params, "body "+op.ReqBodyRef)
		case jsonBody:
			params = append(params, "body any")
		default:
			params = append(params, "req AdvancedRequest")
		}
	}

	multiResp := !raw && len(op.JSONResponses) > 1
	if multiResp {
		pf("// %sResponse is the result of [Client.%s]. Only the JSON field for\n", op.Name, op.Name)
		pf("// StatusCode is set.\n")
		pf("type %sResponse struct {\n\tStatusCode int\n", op.Name)
		for _, r := range op.JSONResponses {
			pf("\tJSON%d *%s\n", r.Code, r.Ref)
		}
		pf("}\n\n")
	}

	// Doc comment.
	pf("// %s", name)
	if op.Summary != "" {
		pf(": %s", op.Summary)
	}
	pf("\n")
	if raw {
		pf("//\n")
		pf("// Requests %s and returns the response unread. The caller must close\n", op.RawAccepts[0])
		pf("// the response body.\n")
	}

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

	fields := []string{
		fmt.Sprintf("method: %q", op.HTTPMethod),
		fmt.Sprintf("pathFmt: %q", pathFmt(op.Path)),
		"pathArgs: " + pathArgsList,
	}
	if op.ParamsType != "" {
		fields = append(fields, "queryParams: params")
	}
	switch {
	case !op.HasBody:
	case jsonBody:
		fields = append(fields, "body: body")
	default:
		fields = append(fields, "rawBody: req.Body", fmt.Sprintf("bodyContentType: cmp.Or(req.ContentType, %q)", op.BodyContentType))
	}
	if raw {
		fields = append(fields, fmt.Sprintf("accept: %q", op.RawAccepts[0]))
	}
	codeStrs := make([]string, len(op.SuccessCodes))
	for i, c := range op.SuccessCodes {
		codeStrs[i] = strconv.Itoa(c)
	}
	fields = append(fields,
		"successCodes: []int{"+strings.Join(codeStrs, ", ")+"}",
		"errorCodes: "+errorCodesExpr,
	)
	reqLit := "apiRequest{\n" + strings.Join(fields, ",\n") + ",\n}"

	sig := strings.Join(params, ", ")
	switch {
	case raw:
		pf("func (c *Client) %s(%s) (*http.Response, error) {\n", name, sig)
		pf("\treturn c.do(ctx, %s)\n", reqLit)
	case multiResp:
		pf("func (c *Client) %s(%s) (*%sResponse, error) {\n", name, sig, op.Name)
		pf("\tstatus, respBody, err := c.doJSONWithStatus(ctx, %s)\n", reqLit)
		pf("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
		pf("\tresult := &%sResponse{StatusCode: status}\n", op.Name)
		pf("\tswitch status {\n")
		for _, r := range op.JSONResponses {
			pf("\tcase %d:\n", r.Code)
			pf("\t\tresult.JSON%d = new(%s)\n", r.Code, r.Ref)
			pf("\t\terr = json.Unmarshal(respBody, result.JSON%d)\n", r.Code)
		}
		pf("\t}\n")
		pf("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
		pf("\treturn result, nil\n")
	case len(op.JSONResponses) == 1:
		ref := op.JSONResponses[0].Ref
		pf("func (c *Client) %s(%s) (*%s, error) {\n", name, sig, ref)
		pf("\treturn doJSON[%s](c, ctx, %s)\n", ref, reqLit)
	default:
		pf("func (c *Client) %s(%s) error {\n", name, sig)
		pf("\treturn c.doNoResponse(ctx, %s)\n", reqLit)
	}

	pf("}\n")
}
