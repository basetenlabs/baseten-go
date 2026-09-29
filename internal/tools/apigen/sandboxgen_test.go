package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestExecutionSnapshot(t *testing.T) {
	data, err := os.ReadFile("specs/sandbox.yml")
	if err != nil {
		t.Fatal(err)
	}
	pre, err := preprocessSpec(data)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = json.Unmarshal(pre.data, &doc); err != nil {
		t.Fatal(err)
	}
	ops, err := extractOperations(doc)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, item := range mapNode(doc["paths"]) {
		for method := range mapNode(item) {
			switch method {
			case "get", "post", "put", "delete", "patch", "head", "options":
				count++
			}
		}
	}
	if len(ops) != count {
		t.Fatalf("generated %d of %d operations", len(ops), count)
	}
	found := map[string]apiOperation{}
	for _, op := range ops {
		found[op.Name] = op
	}
	if !reflect.DeepEqual(found["PostArchiveExport"].SuccessCodes, []int{200, 202}) {
		t.Fatal("archive statuses lost")
	}
	if found["PostArchiveExport"].Responses[202] != "ExportProgress" {
		t.Fatal("archive progress payload lost")
	}
	if !found["PutFilesystemMultipartPart"].NonJSONBody || found["PutFilesystemMultipartPart"].ParamsType == "" {
		t.Fatal("multipart plus query lost")
	}
	if !found["GetWatchFilesystem"].RawResponse {
		t.Fatal("watch stream lost")
	}
	dst := filepath.Join(t.TempDir(), "client.go")
	if err = generateClient(pre.data, dst, "sandboxapi"); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(dst)
	for i := 0; i < 5; i++ {
		if err = generateClient(pre.data, dst, "sandboxapi"); err != nil {
			t.Fatal(err)
		}
		next, _ := os.ReadFile(dst)
		if string(first) != string(next) {
			t.Fatal("nondeterministic generation")
		}
	}
}

func TestYAMLInlineSchemasAndCollisions(t *testing.T) {
	pre, err := preprocessSpec([]byte(`openapi: 3.0.0
info: {title: test, version: v1}
components:
  schemas:
    foo.bar: {type: object}
paths:
  /items:
    post:
      requestBody:
        content:
          application/json:
            schema: {type: object, properties: {name: {type: string}}}
      responses:
        '200':
          description: ok
          content:
            application/json:
              schema: {type: array, items: {$ref: '#/components/schemas/foo.bar'}}
        '204': {description: empty}
`))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal(pre.data, &doc)
	ops, err := extractOperations(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].ReqBodyRef == "" || ops[0].Responses[200] == "" || ops[0].Responses[204] != "" {
		t.Fatalf("incorrect inline/empty responses: %+v", ops)
	}
	if strings.Contains(string(pre.data), "foo.bar") {
		t.Fatal("unnormalized reference")
	}
	_, err = preprocessSpec([]byte(`{"components":{"schemas":{"foo.bar":{},"FooBar":{}}}}`))
	if err == nil {
		t.Fatal("collision accepted")
	}
}

func TestExistingInferenceSignatures(t *testing.T) {
	data, err := os.ReadFile("specs/inference.json")
	if err != nil {
		t.Fatal(err)
	}
	pre, err := preprocessSpec(data, false)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal(pre.data, &doc)
	ops, err := extractOperations(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if op.Name == "AsyncPredict" && op.RespRef != "AsyncPredictOutput" {
			t.Fatalf("changed return type: %s", op.RespRef)
		}
	}
}

// Compile and execute the generated fixture: assertions exercise HTTP behavior,
// not just source strings, and catch disagreement with oapi-codegen model names.
func TestGeneratedMixedSuccessAndParameterOverrides(t *testing.T) {
	dir := t.TempDir()
	spec := filepath.Join(dir, "fixture.yml")
	const fixture = `openapi: 3.0.0
info: {title: fixture, version: '1'}
components:
  parameters:
    Flag: {name: flag, in: query, schema: {type: string}}
    HeaderToken: {name: X-Trace, in: header, schema: {type: string}}
paths:
  /headers:
    parameters:
      - {$ref: '#/components/parameters/HeaderToken'}
    get:
      parameters:
        - {name: X-First, in: header, required: true, schema: {type: array, items: {type: integer}}}
        - {name: X-Second, in: header, required: true, schema: {type: array, items: {type: string}}}
      responses:
        '204': {description: empty}
  /items:
    parameters:
      - {$ref: '#/components/parameters/Flag'}
      - {$ref: '#/components/parameters/HeaderToken'}
    post:
      operationId: UpdateItems
      parameters:
        - {name: flag, in: query, schema: {type: boolean}}
        - {name: X-Trace, in: header, schema: {type: boolean}}
        - {name: count, in: query, schema: {type: integer}}
        - {name: tags, in: query, schema: {type: array, items: {type: string}}}
      requestBody:
        required: false
        content:
          application/json:
            schema: {type: object, properties: {name: {type: string}}}
      responses:
        '200':
          description: ok
          content:
            application/json:
              schema: {type: object, properties: {name: {type: string}}}
        '204': {description: empty}
`
	if err := os.WriteFile(spec, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = generateAPI(cwd, spec, dir, "sandboxapi"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture\n\ngo 1.25.0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	const runtimeTest = `package sandboxapi
import("context";"io";"net/http";"net/http/httptest";"testing")
func TestWire(t *testing.T){for _,status:=range []int{200,204}{server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){if r.Header.Get("X-Trace")!="false"||r.URL.Query().Has("X-Trace"){t.Error("header lost or leaked into query")};if r.URL.Query().Get("flag")!="false"||r.URL.Query().Get("count")!="0"||len(r.URL.Query()["tags"])!=2{t.Errorf("query changed: %s",r.URL)};body,_:=io.ReadAll(r.Body);if string(body)!="{}"{t.Errorf("body changed: %s",body)};w.Header().Set("Content-Type","application/json");w.WriteHeader(status);if status==200{io.WriteString(w,"{\"name\":\"ok\"}")}}));flag:=false;count:=0;tags:=[]string{"a","b"};c:=&Client{BaseURL:server.URL,HTTPClient:server.Client()};response,err:=c.UpdateItems(context.Background(),UpdateItemsParams{Flag:&flag,Count:&count,Tags:&tags,XTrace:&flag},PostItemsBody{});if err!=nil{t.Fatal(err)};if response.StatusCode!=status{t.Fatal(response.StatusCode)};if status==200&&response.JSON200==nil{t.Fatal("missing JSON")};if status==204&&response.JSON200!=nil{t.Fatal("decoded empty body")};if response.Body!=nil{response.Body.Close()};server.Close()}}
func TestHeaderOnly(t *testing.T){server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){if r.Header.Get("X-Trace")!="selected"||r.Header.Get("X-First")!="0,1"||r.Header.Get("X-Second")!="a,b"||r.URL.RawQuery!=""{t.Errorf("bad header-only request: %v %s",r.Header,r.URL)};w.WriteHeader(204)}));defer server.Close();c:=&Client{BaseURL:server.URL,HTTPClient:server.Client()};trace:="selected";if err:=c.GetHeaders(context.Background(),GetHeadersParams{XTrace:&trace,XFirst:[]int{0,1},XSecond:[]string{"a","b"}});err!=nil{t.Fatal(err)};ignored:="ignored";resp,err:=c.GetHeadersRaw(context.Background(),GetHeadersParams{XTrace:&ignored,XFirst:[]int{0,1},XSecond:[]string{"a","b"}},RawRequestOptions{Headers:http.Header{"X-Trace":[]string{"selected"}}});if err!=nil{t.Fatal(err)};resp.Body.Close()}
`
	if err = os.WriteFile(filepath.Join(dir, "sandboxapi", "wire_test.go"), []byte(runtimeTest), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "./...")
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated client: %v\n%s", err, output)
	}
}
